// Package ui is the Bubble Tea front end. Every network call runs in a
// tea.Cmd; Update only touches in-memory state, so keys never wait on I/O.
// Lists are cached per view and shown immediately on revisit while a
// background refresh runs (stale-while-revalidate).
package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ate-tooling/ate-tui/internal/proxy"
	tea "github.com/charmbracelet/bubbletea"
)

// ConnInfo describes the connected cluster for the header.
type ConnInfo struct {
	Context  string
	Cluster  string
	Endpoint string
}

// Options configures the UI.
type Options struct {
	// Connect runs in the background after the first frame is drawn, and
	// again for each :ctx switch. An empty kubeContext is Options.Context.
	Connect func(ctx context.Context, kubeContext string) (Backend, ConnInfo, error)
	// Contexts lists the kubeconfig contexts for :ctx; nil disables it.
	Contexts func() ([]KubeContext, error)
	// Context is the kube context to start on; empty is the current one.
	Context   string
	Refresh   time.Duration
	Version   string
	Kind      string
	Atespace  string
	Filter    string
	ConnLabel string

	// Write enables writes on any context; WriteContexts enables them on
	// the listed kube contexts only. Otherwise ate-tui is read-only.
	Write         bool
	WriteContexts []string
	// ConfigPath is shown in the read-only hint.
	ConfigPath string
	// JournalPath is where action results are appended; empty disables it.
	JournalPath string

	// Keys rebinds table keys; the zero Keymap keeps the defaults.
	Keys Keymap
	// ProxyAddress is where forwards listen; empty is 127.0.0.1.
	ProxyAddress string
	// Copy puts text on the clipboard (OSC 52); nil disables copy.
	Copy func(string) error
}

type mode int

const (
	modeTable mode = iota
	modeFilter
	modeCommand
	modePager
	modeConfirm
	modeForward
)

type viewKey struct {
	kind  kind
	scope string
}

type (
	connectedMsg struct {
		epoch   int
		backend Backend
		info    ConnInfo
		err     error
	}
	fetchedMsg struct {
		key  viewKey
		gen  int
		rows []row
		err  error
		at   time.Time
	}
	tickMsg time.Time
)

// Model is the root Bubble Tea model.
type Model struct {
	opts Options

	backend    Backend
	info       ConnInfo
	connErr    error
	connecting bool
	// kubeCtx is the context being connected to; epoch counts connects so
	// a slow one from an earlier context is dropped.
	kubeCtx     string
	epoch       int
	contexts    []KubeContext
	contextsErr error

	width, height int
	mode          mode
	keys          Keymap
	lastClick     time.Time
	// edit runs the editor on a file; nil runs $EDITOR. Tests set it.
	edit func(path string, done func(error) tea.Msg) tea.Cmd
	// wide shows every column and hides none on a narrow terminal (w).
	wide bool

	cur      viewKey
	history  []viewKey
	histIdx  int
	atespace string
	worker   string // scope of the assignments and history views

	data   map[viewKey]*dataset
	tables map[viewKey]*tableState
	gen    int

	input       string
	suggestions []string
	suggestIdx  int

	pager *pager
	logs  *logStream
	pulse *pulseData
	// want is where a relation jump puts the cursor once the row is listed.
	want wantRow

	timeline  []tlEvent
	lastState map[string]string

	status    string
	statusErr bool
	// fetchErr is set while the status shows a list error, which the next
	// good list clears. Action results stay until the next status.
	fetchErr bool

	confirm *confirm
	batches map[int]*actionBatch

	fwd        *fwdDialog
	forwards   []*proxy.Forward
	fwdPending int
	repoll     time.Duration

	started       time.Time
	session       []journalEntry
	prior         []journalEntry
	journalLoaded bool
	journalErr    error
}

// New builds the model. Nothing touches the network until Init's commands run.
func New(opts Options) *Model {
	if opts.Refresh <= 0 {
		opts.Refresh = 2 * time.Second
	}
	m := &Model{
		opts:       opts,
		keys:       opts.Keys,
		connecting: true,
		atespace:   opts.Atespace,
		kubeCtx:    opts.Context,
		data:       map[viewKey]*dataset{},
		tables:     map[viewKey]*tableState{},
		lastState:  map[string]string{},
		batches:    map[int]*actionBatch{},
		repoll:     300 * time.Millisecond,
		started:    time.Now(),
	}
	k := lookupKind(opts.Kind)
	if k == nil {
		k = lookupKind(string(kActors))
	}
	m.cur = m.keyFor(k.kind)
	m.history = []viewKey{m.cur}
	m.table().filter = opts.Filter
	m.setStatus("Connecting to "+opts.ConnLabel+"…", false)
	return m
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.connect(), m.tick())
}

func (m *Model) connect() tea.Cmd {
	m.connecting = true
	m.epoch++
	connect, name, epoch := m.opts.Connect, m.kubeCtx, m.epoch
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b, info, err := connect(ctx, name)
		return connectedMsg{epoch: epoch, backend: b, info: info, err: err}
	}
}

// Close stops the forwards and closes the connection. main calls it once
// the program has exited.
func (m *Model) Close() {
	m.StopForwards()
	if c, ok := m.backend.(interface{ Close() }); ok {
		c.Close()
	}
	m.backend = nil
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.opts.Refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) keyFor(k kind) viewKey {
	switch {
	case workerScoped(k):
		return viewKey{k, m.worker}
	case lookupKind(string(k)).scoped:
		return viewKey{k, m.atespace}
	}
	return viewKey{k, ""}
}

func (m *Model) def() *kindDef { return lookupKind(string(m.cur.kind)) }

// columns are a kind's columns, with its wide ones in wide mode.
func (m *Model) columns(k kind) []string {
	def := lookupKind(string(k))
	if !m.wide || len(def.wide) == 0 {
		return def.columns
	}
	return append(slices.Clip(def.columns), def.wide...)
}

func (m *Model) ds(k viewKey) *dataset {
	d := m.data[k]
	if d == nil {
		d = &dataset{}
		m.data[k] = d
	}
	return d
}

func (m *Model) table() *tableState { return m.tableFor(m.cur) }

func (m *Model) tableFor(k viewKey) *tableState {
	t := m.tables[k]
	if t == nil {
		t = &tableState{marked: map[string]bool{}}
		if def := lookupKind(string(k.kind)); def != nil {
			t.sortCol = def.sortCol
		}
		m.tables[k] = t
	}
	return t
}

// fetch starts a background list unless one is already in flight.
func (m *Model) fetch(k viewKey) tea.Cmd {
	if m.backend == nil {
		return nil
	}
	d := m.ds(k)
	if d.loading {
		return nil
	}
	d.loading = true
	m.gen++
	d.gen = m.gen
	gen, b, def := m.gen, m.backend, lookupKind(string(k.kind))
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		now := time.Now()
		rows, err := def.fetch(ctx, b, k.scope, now)
		return fetchedMsg{key: k, gen: gen, rows: rows, err: err, at: now}
	}
}

// setView switches the table to k, showing cached rows at once.
func (m *Model) setView(k viewKey, pushHistory bool) tea.Cmd {
	m.closePager()
	m.mode, m.confirm, m.fwd = modeTable, nil, nil
	if k != m.cur {
		clear(m.table().marked)
	}
	m.cur = k
	if m.want.key != k {
		m.want = wantRow{}
	}
	if pushHistory {
		m.history = append(m.history[:m.histIdx+1], k)
		m.histIdx = len(m.history) - 1
	}
	t := m.table()
	if d := m.data[k]; d != nil {
		t.rebuild(d.rows, m.columns(k.kind), m.keepID(m.selectedID()))
		t.clamp(m.tableHeight())
		if m.selectedID() == m.want.id {
			m.want = wantRow{}
		}
	}
	m.setStatus("Viewing "+string(k.kind), false)
	return m.fetch(k)
}

// keepID is the row the cursor should stay on: a pending jump's target in
// this view, or the current row.
func (m *Model) keepID(cur string) string {
	if m.want.id != "" && m.want.key == m.cur {
		return m.want.id
	}
	return cur
}

func (m *Model) gotoKind(k kind, filter string) tea.Cmd {
	key := m.keyFor(k)
	t := m.tableFor(key)
	t.filter = filter
	if d := m.data[key]; d != nil {
		t.rebuild(d.rows, m.columns(k), "")
	}
	return m.setView(key, true)
}

func (m *Model) setAtespace(a string) tea.Cmd {
	m.atespace = a
	k := m.cur.kind
	if !lookupKind(string(k)).scoped {
		k = kActors
	}
	return m.setView(m.keyFor(k), true)
}

func (m *Model) setStatus(s string, isErr bool) {
	m.status, m.statusErr, m.fetchErr = s, isErr, false
}

func (m *Model) selected() *row {
	d := m.data[m.cur]
	t := m.table()
	if d == nil || t.cursor >= len(t.visible) || len(t.visible) == 0 {
		return nil
	}
	return &d.rows[t.visible[t.cursor]]
}

func (m *Model) selectedID() string {
	if r := m.selected(); r != nil {
		return r.id
	}
	return ""
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		resized := msg.Width != m.width
		m.width, m.height = msg.Width, msg.Height
		m.table().clamp(m.tableHeight())
		if resized && m.logs != nil && m.logs.wrap {
			m.rebuildLogs()
		}
		if m.pager != nil && m.pager.resize != nil {
			m.pager.resize()
		}
		return m, nil

	case connectedMsg:
		if msg.epoch != m.epoch {
			// A connect to a context we have since left.
			if c, ok := msg.backend.(interface{ Close() }); ok {
				go c.Close()
			}
			return m, nil
		}
		m.connecting = false
		if msg.err != nil {
			m.connErr = msg.err
			m.setStatus("connect: "+msg.err.Error()+"  (r to retry)", true)
			return m, nil
		}
		m.connErr = nil
		m.backend, m.info = msg.backend, msg.info
		m.setStatus("Viewing "+string(m.cur.kind), false)
		// Warm the views people jump to first, all in parallel.
		return m, tea.Batch(m.fetch(m.cur), m.fetch(viewKey{kAtespaces, ""}),
			m.fetch(viewKey{kWorkers, ""}), m.fetch(viewKey{kActors, m.atespace}), m.loadContexts())

	case contextsMsg:
		m.contextsLoaded(msg)
		return m, nil

	case fetchedMsg:
		var cmd tea.Cmd
		d := m.ds(msg.key)
		if msg.gen != d.gen {
			return m, nil
		}
		// Remember the selection against the old rows so the cursor stays on
		// the same object after the refresh.
		keep := ""
		if msg.key == m.cur {
			keep = m.keepID(m.selectedID())
		}
		d.loading = false
		d.fetchedAt = msg.at
		d.err = msg.err
		if msg.err == nil {
			d.rows = msg.rows
			if msg.key.kind == kActors {
				m.recordTimeline(msg.rows, msg.at)
			}
		}
		if m.pulse != nil && pulseKeys[msg.key] {
			cmd = m.pulseRefresh()
		}
		if msg.key == m.cur {
			if msg.err != nil {
				m.setStatus(string(msg.key.kind)+": "+errString(msg.err), true)
				m.fetchErr = true
			} else if m.fetchErr {
				m.setStatus("Viewing "+string(m.cur.kind), false)
			}
			t := m.table()
			t.rebuild(d.rows, m.columns(msg.key.kind), keep)
			t.clamp(m.tableHeight())
			if m.want.id != "" && m.selectedID() == m.want.id {
				m.want = wantRow{}
			}
		}
		return m, cmd

	case tickMsg:
		cmds := []tea.Cmd{m.tick()}
		if m.backend != nil {
			if !m.def().manual {
				cmds = append(cmds, m.fetch(m.cur))
			}
			if m.pager != nil && m.pager.reload != nil && !m.pager.loading {
				cmds = append(cmds, m.pager.reload())
			}
			cmds = append(cmds, m.pollLogActor())
		}
		return m, tea.Batch(cmds...)

	case pagerMsg:
		if m.pager == nil || msg.gen != m.pager.gen {
			return m, nil
		}
		m.pager.loading = false
		if msg.err != nil {
			m.pager.setLines([]string{stStatusErr.Render(errString(msg.err))}, m.pagerHeight())
		} else {
			m.pager.setLines(msg.lines, m.pagerHeight())
		}
		return m, nil

	case logChunkMsg:
		return m, m.logChunk(msg)

	case logActorMsg:
		return m, m.logActor(msg)

	case relationsMsg:
		m.relationsLoaded(msg)
		return m, nil

	case pulseMsg:
		m.pulseLoaded(msg)
		return m, nil

	case actionResultMsg:
		return m, m.actionResult(msg)

	case repollMsg:
		if m.backend == nil {
			return m, nil
		}
		return m, m.fetch(msg.key)

	case fwdStartedMsg:
		return m, m.forwardStarted(msg)

	case fwdStoppedMsg:
		return m, nil

	case editMsg:
		return m, m.openEditor(msg)

	case editedMsg:
		return m, m.edited(msg)

	case manifestMsg:
		return m, m.manifestParsed(msg)

	case copiedMsg:
		if msg.err != nil {
			m.setStatus("copy: "+msg.err.Error(), true)
		} else {
			m.setStatus("Copied "+msg.what, false)
		}
		return m, nil

	case journalMsg:
		if msg.err != nil {
			m.setStatus(m.status+"  (journal: "+msg.err.Error()+")", true)
		}
		return m, nil

	case journalLoadedMsg:
		m.journalLoaded, m.prior, m.journalErr = true, msg.entries, msg.err
		if m.pager != nil && msg.gen == m.pager.gen {
			m.pager.loading = false
			m.pager.setLines(m.journalLines(), m.pagerHeight())
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(msg)

	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	}
	return m, nil
}

func errString(err error) string {
	s := err.Error()
	// gRPC errors read "rpc error: code = X desc = Y"; Y is what matters.
	if i := strings.Index(s, "desc = "); i >= 0 {
		s = s[i+len("desc = "):]
	}
	return s
}

func (m *Model) tableHeight() int {
	h := m.height - headerHeight - footerHeight - 3 // borders + column header
	if m.mode == modeCommand {
		h -= 3
	}
	return max(h, 1)
}

func (m *Model) pagerHeight() int {
	return max(m.height-headerHeight-footerHeight-2, 1)
}

// tlEvent is one observed actor state change.
type tlEvent struct {
	at       time.Time
	id       string
	from, to string
	worker   string
}

const maxTimeline = 5000

// recordTimeline diffs actor states against the previous poll.
func (m *Model) recordTimeline(rows []row, at time.Time) {
	for i := range rows {
		r := &rows[i]
		state := r.cells[2] + "@" + r.cells[4]
		prev, seen := m.lastState[r.id]
		if seen && prev == state {
			continue
		}
		m.lastState[r.id] = state
		from := ""
		if seen {
			from = strings.SplitN(prev, "@", 2)[0]
		}
		m.timeline = append(m.timeline, tlEvent{at: at, id: r.id, from: from, to: r.cells[2], worker: r.cells[4]})
	}
	if over := len(m.timeline) - maxTimeline; over > 0 {
		m.timeline = append(m.timeline[:0], m.timeline[over:]...)
	}
}

func fmtCount(visible, total int) string {
	if visible == total {
		return fmt.Sprint(total)
	}
	return fmt.Sprintf("%d/%d", visible, total)
}
