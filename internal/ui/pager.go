package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"
)

// pager is a scrollable text view over the table area.
type pager struct {
	title   string
	lines   []string
	offset  int
	follow  bool
	loading bool
	gen     int
	// reload refreshes the content on each tick; nil for static views.
	reload func() tea.Cmd
}

type pagerMsg struct {
	gen   int
	lines []string
	err   error
}

func (p *pager) setLines(lines []string, h int) {
	p.lines = lines
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, h)
}

const maxLogLines = 20000

func (p *pager) appendLines(lines []string, h int) {
	p.lines = append(p.lines, lines...)
	if over := len(p.lines) - maxLogLines; over > 0 {
		p.lines = append(p.lines[:0], p.lines[over:]...)
		p.offset -= over
	}
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, h)
}

func (p *pager) scroll(n, h int) {
	p.offset += n
	if max := len(p.lines) - h; p.offset > max {
		p.offset = max
	}
	if p.offset < 0 {
		p.offset = 0
	}
	// Scrolling up pauses log follow; reaching the end resumes it.
	if n < 0 {
		p.follow = false
	}
}

func (m *Model) newPager(title string) *pager {
	m.closePager()
	m.gen++
	m.pager = &pager{title: title, gen: m.gen, loading: true, lines: []string{stDim.Render("loading…")}}
	m.mode = modePager
	return m.pager
}

func (m *Model) closePager() {
	if m.logs != nil {
		m.logs.cancel()
		m.logs = nil
	}
	m.pager = nil
}

// lookup finds the freshest copy of a row so live views follow updates.
func (m *Model) lookup(k viewKey, id string) any {
	if d := m.data[k]; d != nil {
		for i := range d.rows {
			if d.rows[i].id == id {
				return d.rows[i].obj
			}
		}
	}
	return nil
}

func (m *Model) openYAML(k viewKey, id string) tea.Cmd {
	p := m.newPager(stTitleKind.Render("yaml") + " " + stTitleCount.Render(id))
	gen := p.gen
	p.reload = func() tea.Cmd {
		obj := m.lookup(k, id)
		if obj == nil {
			return nil
		}
		return func() tea.Msg {
			lines, err := toYAML(obj)
			return pagerMsg{gen: gen, lines: lines, err: err}
		}
	}
	return p.reload()
}

func toYAML(obj any) ([]string, error) {
	var out []byte
	var err error
	switch o := obj.(type) {
	case proto.Message:
		var j []byte
		j, err = protojson.MarshalOptions{UseProtoNames: true}.Marshal(o)
		if err == nil {
			out, err = yaml.JSONToYAML(j)
		}
	case *infrav1.WorkerPool:
		c := o.DeepCopy()
		c.ManagedFields = nil
		out, err = yaml.Marshal(c)
	default:
		err = fmt.Errorf("no yaml for %T", obj)
	}
	if err != nil {
		return nil, err
	}
	raw := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = colorYAML(l)
	}
	return lines, nil
}

func colorYAML(l string) string {
	trimmed := strings.TrimLeft(l, " ")
	indent := l[:len(l)-len(trimmed)]
	dash := ""
	if strings.HasPrefix(trimmed, "- ") {
		dash, trimmed = stYAMLDash.Render("- "), trimmed[2:]
	}
	if i := strings.Index(trimmed, ":"); i > 0 && !strings.ContainsAny(trimmed[:i], " \"'") {
		return indent + dash + stYAMLKey.Render(trimmed[:i+1]) + stYAMLVal.Render(trimmed[i+1:])
	}
	return indent + dash + stYAMLVal.Render(trimmed)
}

func (m *Model) openExplain(k viewKey, id string) tea.Cmd {
	p := m.newPager(stTitleKind.Render("explain") + " " + stTitleCount.Render(id))
	gen := p.gen
	b := m.backend
	p.reload = func() tea.Cmd {
		obj := m.lookup(k, id)
		if obj == nil || b == nil {
			return nil
		}
		m.pager.loading = true
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, err := buildReport(ctx, b, obj)
			if err != nil {
				return pagerMsg{gen: gen, err: err}
			}
			return pagerMsg{gen: gen, lines: renderReport(r)}
		}
	}
	return p.reload()
}

// buildReport fetches what an explanation needs, in parallel.
func buildReport(ctx context.Context, b Backend, obj any) (explain.Report, error) {
	now := time.Now()
	switch o := obj.(type) {
	case *ateapi.Actor:
		in := explain.ActorInput{Actor: o, Now: now}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			in.Template, in.TemplateErr = b.GetActorTemplate(ctx, o.GetActorTemplate())
		}()
		go func() {
			defer wg.Done()
			in.Workers, _ = b.ListWorkers(ctx)
		}()
		wg.Wait()
		return explain.Actor(in), nil
	case *ateapi.Worker:
		as, err := b.ListWorkerAssignments(ctx, o.GetMetadata().GetName())
		if err != nil {
			return explain.Report{}, err
		}
		return explain.Worker(explain.WorkerInput{Worker: o, Assignments: as, Now: now}), nil
	case *ateapi.ActorTemplate:
		as, err := b.ListActors(ctx, o.GetMetadata().GetAtespace())
		if err != nil {
			return explain.Report{}, err
		}
		return explain.Template(explain.TemplateInput{Template: o, Actors: as, Now: now}), nil
	case *infrav1.WorkerPool:
		ws, _ := b.ListWorkers(ctx)
		return explain.WorkerPool(o, ws, now), nil
	}
	return explain.Report{}, fmt.Errorf("explain is not available for %T", obj)
}

var levelGlyph = map[explain.Level]string{
	explain.OK:    fg(cGreen).Render("✔"),
	explain.Info:  fg(cSky).Render("•"),
	explain.Warn:  fg(cPeach).Render("▲"),
	explain.Error: fg(cRed).Render("✖"),
}

var levelStyle = map[explain.Level]func(...string) string{
	explain.OK:    fg(cGreen).Render,
	explain.Info:  fg(cText).Render,
	explain.Warn:  fg(cPeach).Render,
	explain.Error: fg(cRed).Render,
}

func renderReport(r explain.Report) []string {
	var out []string
	out = append(out, stSection.Render(r.Subject)+"  "+levelStyle[r.Level](r.Summary), "")
	w := 0
	for _, f := range r.Facts {
		w = max(w, len(f.Label))
	}
	for _, f := range r.Facts {
		out = append(out, stLabel.Render(fmt.Sprintf("%-*s", w+2, f.Label))+stCount.Render(f.Value))
	}
	out = append(out, "", stSection.Render("Diagnosis"))
	for _, f := range r.Findings {
		out = append(out, levelGlyph[f.Level]+" "+levelStyle[f.Level](f.Title))
		for _, d := range f.Detail {
			out = append(out, "    "+stDim.Render(d))
		}
	}
	if len(r.Next) > 0 {
		out = append(out, "", stSection.Render("Next"))
		for _, n := range r.Next {
			out = append(out, stDim.Render("$ ")+fg(cSky).Render(n))
		}
	}
	return out
}

func (m *Model) openTimeline(id string) {
	title := "timeline"
	if id != "" {
		title += " " + stTitleCount.Render(id)
	}
	p := m.newPager(stTitleKind.Render(title))
	p.follow = true
	build := func() []string {
		var out []string
		for _, e := range m.timeline {
			if id != "" && e.id != id {
				continue
			}
			change := fg(cGreen).Render("seen ") + e.to
			if e.from != "" {
				change = e.from + stDim.Render(" → ") + e.to
			}
			line := stLogTime.Render(e.at.Format("15:04:05")) + "  "
			if id == "" {
				line += fg(cBlue).Render(fmt.Sprintf("%-40s", e.id)) + " "
			}
			line += change
			if e.worker != "-" {
				line += stDim.Render("  on " + e.worker)
			}
			out = append(out, line)
		}
		if len(out) == 0 {
			out = []string{stDim.Render("no changes observed yet; the timeline records what each poll sees while ate-tui runs")}
		}
		return out
	}
	p.loading = false
	p.setLines(build(), m.pagerHeight())
	gen := p.gen
	p.reload = func() tea.Cmd {
		lines := build()
		return func() tea.Msg { return pagerMsg{gen: gen, lines: lines} }
	}
}

func (m *Model) openHelp() {
	p := m.newPager(stTitleKind.Render("help"))
	p.loading = false
	sections := []struct {
		name  string
		hints []hint
	}{
		{"Navigation", []hint{
			{":<kind> [atespace]", "switch view (actors, workers, templates, workerpools, atespaces, tags)"},
			{":atespace <name>", "scope to an atespace; 0 or :all for every atespace"},
			{"tab / shift-tab", "cycle common views"},
			{"[ / ]", "back / forward through view history"},
			{"j k g G", "move; ctrl-f / ctrl-b page"},
			{"/", "filter rows (space = AND, !term = NOT); esc clears"},
			{"ctrl-z", "toggle faults only (crashed, stuck, draining, not ready)"},
			{"S / I", "cycle sort column / invert"},
			{"r", "refresh now (or reconnect)"},
			{"q, ctrl-c", "quit"},
		}},
		{"Actors", []hint{
			{"⏎ / X", "explain: state, crash, snapshots, placement, template"},
			{"y", "yaml"},
			{"l", "follow logs from the actor's worker pod (f toggles follow)"},
			{"T", "timeline of state changes seen by ate-tui; :timeline for all"},
			{"o / t", "jump to worker / template"},
		}},
		{"Workers & pools", []hint{
			{"⏎", "worker: actor assignments · pool: its workers"},
			{"X", "explain capacity, epochs, sandbox config"},
			{"p", "worker: jump to its pool"},
		}},
		{"Actions (need --write or writeContexts in the config)", []hint{
			{"space", "mark / unmark a row; actions apply to marked rows, or the current one"},
			{"s", "actor: suspend (tab in the dialog: async or sync durability)"},
			{"R", "actor: resume"},
			{"v", "actor: revert to its durable snapshot (clears a crash)"},
			{"ctrl-d", "actor: delete; type the name (or yes for several) to confirm"},
			{"D", "worker: drain (one-way)"},
			{"A / U", "worker: mark available / unavailable"},
			{":journal", "actions and results, this session and earlier"},
		}},
	}
	var lines []string
	for _, s := range sections {
		lines = append(lines, stSection.Render(s.name))
		for _, h := range s.hints {
			lines = append(lines, "  "+stHintKey.Render(fmt.Sprintf("%-22s", h.key))+stHintDesc.Render(h.desc))
		}
		lines = append(lines, "")
	}
	p.setLines(lines, m.pagerHeight())
}

// logStream follows one actor's lines from its worker pod.
type logStream struct {
	gen    int
	ch     chan string
	cancel context.CancelFunc
}

type logChunkMsg struct {
	gen   int
	lines []string
	done  bool
}

// wait blocks off the UI goroutine for the next lines, then drains whatever
// else is buffered so a burst renders in one frame.
func (s *logStream) wait() tea.Cmd {
	ch, gen := s.ch, s.gen
	return func() tea.Msg {
		l, ok := <-ch
		if !ok {
			return logChunkMsg{gen: gen, done: true}
		}
		lines := []string{l}
		for len(lines) < 2000 {
			select {
			case l, ok := <-ch:
				if !ok {
					return logChunkMsg{gen: gen, lines: lines, done: true}
				}
				lines = append(lines, l)
			default:
				return logChunkMsg{gen: gen, lines: lines}
			}
		}
		return logChunkMsg{gen: gen, lines: lines}
	}
}

func (m *Model) openLogs(a *ateapi.Actor) tea.Cmd {
	md := a.GetMetadata()
	id := md.GetAtespace() + "/" + md.GetName()
	p := m.newPager(stTitleKind.Render("logs") + " " + stTitleCount.Render(id))
	p.follow = true
	wa := a.GetStatus().GetWorkerAssignment()
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || wa.GetWorkerPod() == "" {
		p.loading = false
		p.setLines([]string{stDim.Render("actor is " + explain.ActorState(a.GetStatus().GetState()) + " and not on a worker; logs are only available while it runs")}, m.pagerHeight())
		return nil
	}
	p.lines = nil
	p.title += stDim.Render("  " + wa.GetWorkerNamespace() + "/" + wa.GetWorkerPod())

	ctx, cancel := context.WithCancel(context.Background())
	s := &logStream{gen: p.gen, ch: make(chan string, 4096), cancel: cancel}
	m.logs = s
	b := m.backend
	go func() {
		defer close(s.ch)
		send := func(l string) bool {
			select {
			case s.ch <- l:
				return true
			case <-ctx.Done():
				return false
			}
		}
		rc, err := b.PodLogs(ctx, wa.GetWorkerNamespace(), wa.GetWorkerPod(), true, 5000)
		if err != nil {
			send(stStatusErr.Render("logs: " + err.Error()))
			return
		}
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if l, ok := formatLogLine(sc.Bytes(), md.GetAtespace(), md.GetName()); ok && !send(l) {
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			send(stStatusErr.Render("logs: " + err.Error()))
		}
	}()
	return s.wait()
}

// Log label keys from go.ate.dev/substrate/internal/ateattr.
const (
	labelAtespace  = "ate.atespace"
	labelActor     = "ate.actor.name"
	labelContainer = "ate.actor.container.name"
	sandboxID      = "_sandbox"
)

// formatLogLine keeps lines the worker attributes to this actor. A worker pod
// can host several actors over time, so lines must match both labels.
func formatLogLine(b []byte, atespace, name string) (string, bool) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "", false
	}
	var labels map[string]any
	for _, k := range []string{"logging.googleapis.com/labels", "labels"} {
		if l, ok := m[k].(map[string]any); ok {
			labels = l
			break
		}
	}
	if labels[labelActor] != name || labels[labelAtespace] != atespace {
		return "", false
	}
	container, _ := labels[labelContainer].(string)
	if container == sandboxID {
		return "", false
	}
	msg, _ := m["message"].(string)
	if msg == "" {
		msg, _ = m["msg"].(string)
	}
	ts := ""
	for _, k := range []string{"time", "timestamp", "ts"} {
		if s, ok := m[k].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				ts = t.Local().Format("15:04:05")
			}
			break
		}
	}
	src := stLogLifecycle.Render("lifecycle")
	if container != "" {
		src = stLogContainer.Render(container)
	}
	return stLogTime.Render(ts) + " " + src + " " + msg, true
}
