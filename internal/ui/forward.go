package ui

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	"github.com/ate-tooling/ate-tui/internal/proxy"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// A forward tunnels a local port to an actor through atenet-router, like
// `kubectl ate proxy actor`. It mints a client certificate and the router
// resumes a suspended actor on connect, so it is gated like a write. The
// port dialog is its confirmation.

// fwdDialog is the open `f` dialog.
type fwdDialog struct {
	actor *ateapi.Actor
	id    string
	err   string
}

type (
	fwdStartedMsg struct {
		id    string
		spec  string
		fws   []*proxy.Forward
		err   error
		start time.Time
	}
	fwdStoppedMsg struct{}
)

// proxyTimeout bounds minting the router client certificate, which waits
// for the podidentity signer.
const proxyTimeout = 3 * time.Minute

func (m *Model) openForward(a *ateapi.Actor) tea.Cmd {
	switch {
	case m.backend == nil:
		m.setStatus("not connected", true)
		return nil
	case !m.writable():
		m.setStatus(m.readOnlyHint(), true)
		return nil
	}
	md := a.GetMetadata()
	m.fwd = &fwdDialog{actor: a, id: md.GetAtespace() + "/" + md.GetName()}
	m.mode, m.input = modeForward, m.freeLocal()+":"+strconv.Itoa(proxy.DefaultRemotePort)
	return nil
}

func (m *Model) proxyAddr() string {
	if m.opts.ProxyAddress != "" {
		return m.opts.ProxyAddress
	}
	return proxy.DefaultAddress
}

// freeLocal is the first local port from 8000 that no forward holds.
func (m *Model) freeLocal() string {
	used := map[int]bool{}
	for _, f := range m.forwards {
		if _, p, err := net.SplitHostPort(f.Local); err == nil {
			n, _ := strconv.Atoi(p)
			used[n] = true
		}
	}
	p := proxy.DefaultLocalPort
	for used[p] {
		p++
	}
	return strconv.Itoa(p)
}

func (m *Model) keyForward(msg tea.KeyMsg) tea.Cmd {
	d := m.fwd
	switch msg.Type {
	case tea.KeyEsc:
		m.fwd, m.mode = nil, modeTable
		m.setStatus("proxy cancelled", false)
		return nil
	case tea.KeyEnter:
		maps, err := proxy.ParseMappings(m.input)
		if err != nil {
			d.err = err.Error()
			return nil
		}
		m.fwd, m.mode = nil, modeTable
		return m.startForward(d, maps)
	}
	d.err = ""
	m.editInput(msg)
	return nil
}

// startForward mints the router certificate and listens, off the UI
// goroutine. Either every mapping listens or none does.
func (m *Model) startForward(d *fwdDialog, maps []proxy.Mapping) tea.Cmd {
	specs := make([]string, len(maps))
	for i, mp := range maps {
		specs[i] = mp.String()
	}
	spec := strings.Join(specs, " ")
	m.fwdPending++
	m.setStatus("proxy "+d.id+" "+spec+": requesting a router client certificate…", false)
	b, id, addr := m.backend, d.id, m.proxyAddr()
	md := d.actor.GetMetadata()
	ref := &ateapi.ObjectRef{Atespace: md.GetAtespace(), Name: md.GetName()}
	dial := func(ctx context.Context, port int) (net.Conn, error) { return b.DialActor(ctx, ref, port) }
	return func() tea.Msg {
		msg := fwdStartedMsg{id: id, spec: spec, start: time.Now()}
		ctx, cancel := context.WithTimeout(context.Background(), proxyTimeout)
		defer cancel()
		if err := b.PrepareProxy(ctx); err != nil {
			msg.err = err
			return msg
		}
		for _, mp := range maps {
			f, err := proxy.Start(id, addr, mp, dial)
			if err != nil {
				for _, f := range msg.fws {
					f.Stop()
				}
				msg.fws, msg.err = nil, err
				return msg
			}
			msg.fws = append(msg.fws, f)
		}
		return msg
	}
}

func (m *Model) forwardStarted(msg fwdStartedMsg) tea.Cmd {
	m.fwdPending--
	e := journalEntry{Time: msg.start, Context: m.info.Context, Action: "proxy", Mode: msg.spec, Object: msg.id, Result: "ok"}
	if msg.err != nil {
		e.Result, e.Error = "error", proxyErr(msg.err)
		m.setStatus("proxy "+msg.id+": "+e.Error, true)
		return m.record(e)
	}
	m.forwards = append(m.forwards, msg.fws...)
	var parts []string
	for _, f := range msg.fws {
		parts = append(parts, fmt.Sprintf("%s → %d", f.Local, f.Remote))
	}
	m.setStatus("Proxying "+strings.Join(parts, ", ")+" on "+msg.id+"  (:pf lists forwards)", false)
	if m.pager != nil && m.pager.forwards {
		m.drawForwards()
	}
	return m.record(e)
}

// proxyErr is errString plus a hint for missing RBAC on the certificate.
func proxyErr(err error) string {
	s := errString(err)
	if strings.Contains(s, "podcertificaterequests") && strings.Contains(s, "forbidden") {
		s += " (proxy needs create, get and delete on podcertificaterequests in Substrate's system namespace)"
	}
	return s
}

// stopForward removes a forward from the list now and closes it in the
// background.
func (m *Model) stopForward(f *proxy.Forward) tea.Cmd {
	m.forwards = slices.DeleteFunc(m.forwards, func(g *proxy.Forward) bool { return g == f })
	m.setStatus(fmt.Sprintf("stopped %s → %s:%d", f.Local, f.Actor, f.Remote), false)
	e := journalEntry{Time: time.Now(), Context: m.info.Context, Action: "proxy stop",
		Mode: fwdSpec(f), Object: f.Actor, Result: "ok"}
	return tea.Batch(m.record(e), func() tea.Msg {
		f.Stop()
		return fwdStoppedMsg{}
	})
}

func fwdSpec(f *proxy.Forward) string {
	_, p, _ := net.SplitHostPort(f.Local)
	return p + ":" + strconv.Itoa(f.Remote)
}

// StopForwards closes every forward. main calls it once the UI has exited.
func (m *Model) StopForwards() {
	for _, f := range m.forwards {
		f.Stop()
	}
	m.forwards = nil
}

// activeForwards counts the forwards still listening.
func (m *Model) activeForwards() int {
	n := 0
	for _, f := range m.forwards {
		if !f.Stats().Stopped {
			n++
		}
	}
	return n
}

// openForwards lists the forwards with live counters (:pf).
func (m *Model) openForwards() {
	p := m.newPager(stTitleKind.Render("port-forwards"))
	p.forwards, p.loading = true, false
	p.reload = func() tea.Cmd {
		m.drawForwards()
		return nil
	}
	m.drawForwards()
}

func (m *Model) drawForwards() {
	p := m.pager
	p.cursor = min(p.cursor, max(len(m.forwards)-1, 0))
	p.lines = forwardLines(m.forwards, p.cursor, m.width-4, time.Now())
	h := m.pagerHeight()
	row := p.cursor + 1 // row 0 is the column header
	if row < p.offset+1 {
		p.offset = max(row-1, 0)
	}
	if row >= p.offset+h {
		p.offset = row - h + 1
	}
	p.scroll(0, h)
}

func forwardLines(fws []*proxy.Forward, cursor, width int, now time.Time) []string {
	if len(fws) == 0 {
		return []string{stDim.Render("no port-forwards; press f on an actor to start one")}
	}
	cols := []string{"ACTOR", "LOCAL", "REMOTE", "CONNS", "IN", "OUT", "AGE", "STATUS"}
	cells := make([][]string, len(fws))
	levels := make([]rowLevel, len(fws))
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = len(c)
	}
	for i, f := range fws {
		s := f.Stats()
		status, lv := "listening", lvNormal
		switch {
		case s.Stopped && s.Err != nil:
			status, lv = "stopped: "+s.Err.Error(), lvError
		case s.Stopped:
			status, lv = "stopped", lvGone
		case s.LastErr != "" && now.Sub(s.ErrAt) < time.Minute:
			status, lv = "dial failed "+age(s.ErrAt, now)+" ago", lvWarn
		case s.Active > 0:
			status, lv = "connected", lvProgress
		}
		conns := fmt.Sprintf("%d/%d", s.Active, s.Total)
		if s.Failed > 0 {
			conns += fmt.Sprintf(" (%d failed)", s.Failed)
		}
		cells[i] = []string{f.Actor, f.Local, strconv.Itoa(f.Remote), conns,
			explain.Bytes(s.In), explain.Bytes(s.Out), age(f.Started, now), status}
		levels[i] = lv
		for j, c := range cells[i][:len(cols)-1] {
			w[j] = max(w[j], min(len(c), maxColWidth))
		}
	}
	// Hide AGE, OUT and IN, in that order, until STATUS has room.
	hidden := map[int]bool{}
	used := func() int {
		n := 1
		for j := range len(cols) - 1 {
			if !hidden[j] {
				n += w[j] + 2
			}
		}
		return n
	}
	for _, j := range []int{6, 5, 4} {
		if width-used() >= 16 {
			break
		}
		hidden[j] = true
	}
	line := func(c []string) string {
		var b strings.Builder
		b.WriteByte(' ')
		for j, v := range c {
			if j == len(c)-1 {
				b.WriteString(v)
				break
			}
			if !hidden[j] {
				fmt.Fprintf(&b, "%-*s  ", w[j], v)
			}
		}
		return b.String()
	}
	out := []string{stColHeader.Render(line(cols))}
	for i := range fws {
		l := line(cells[i])
		if i == cursor {
			out = append(out, stSelected.Render(fit(l, width)))
		} else {
			out = append(out, rowStyles[levels[i]].Render(l))
		}
	}
	// The selected forward's last dial error in full, as it is too long
	// for the STATUS column.
	if cursor < len(fws) {
		if s := fws[cursor].Stats(); s.LastErr != "" {
			out = append(out, "", stLabel.Render("last dial error ")+stDim.Render(s.ErrAt.Local().Format("15:04:05")))
			for _, l := range strings.Split(ansi.Wrap(s.LastErr, max(width-2, 10), ""), "\n") {
				out = append(out, " "+stStatusErr.Render(l))
			}
		}
	}
	return out
}

// keyForwards handles the :pf list keys; it reports whether it consumed
// the key.
func (m *Model) keyForwards(key string) (bool, tea.Cmd) {
	p := m.pager
	n := len(m.forwards)
	move := func(d int) {
		if n > 0 {
			p.cursor = min(max(p.cursor+d, 0), n-1)
			m.drawForwards()
		}
	}
	h := m.pagerHeight()
	switch key {
	case "j", "down":
		move(1)
	case "k", "up":
		move(-1)
	case "g", "home":
		move(-n)
	case "G", "end":
		move(n)
	case "ctrl+f", "pgdown":
		move(h)
	case "ctrl+b", "pgup":
		move(-h)
	case "ctrl+d", "d":
		if p.cursor < n {
			cmd := m.stopForward(m.forwards[p.cursor])
			m.drawForwards()
			return true, cmd
		}
	case "enter":
		if p.cursor < n {
			f := m.forwards[p.cursor]
			as, _, _ := strings.Cut(f.Actor, "/")
			return true, m.jump(relation{kind: kActors, scope: as, id: f.Actor})
		}
	default:
		return false, nil
	}
	return true, nil
}

// fwdBadge shows the number of listening forwards in the header.
func (m *Model) fwdBadge() string {
	n := m.activeForwards()
	if n == 0 && m.fwdPending == 0 {
		return ""
	}
	s := fmt.Sprintf("⇄ %d", n)
	if m.fwdPending > 0 {
		s += "…"
	}
	return "  " + stLive.Render(s)
}

// viewForward is the `f` dialog, at most maxH lines tall.
func (m *Model) viewForward(maxH int) []string {
	d := m.fwd
	w := min(m.width-4, 72)
	if w < 16 {
		return nil
	}
	inner := w - 4
	lines := []string{stCount.Bold(true).Render("Forward local ports to actor " + d.id), ""}
	switch st := d.actor.GetStatus().GetState(); st {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		lines = append(lines, fg(cPeach).Render("The actor is suspended; the first connection resumes it."), "")
	default:
		lines = append(lines, fg(cPeach).Render("The actor is "+explain.ActorState(st)+"; the router may refuse connections."), "")
	}
	lines = append(lines,
		stLabel.Render("ports  ")+stInputPrompt.Render("> ")+stInput.Render(m.input)+stInput.Render("█"),
		stDim.Render("[local:]remote, space-separated; local 0 = any free port"),
		stDim.Render("listens on "+m.proxyAddr()))
	if d.err != "" {
		lines = append(lines, stStatusErr.Render(d.err))
	}
	lines = append(lines, "", stDim.Render("⏎ start  ·  ctrl-u clear  ·  esc cancel"))
	for i := range lines {
		lines[i] = fit(lines[i], inner)
	}
	h := min(len(lines), max(maxH-2, 1))
	return box(stInputPrompt.Render("proxy"), lines, w, h, fg(cPeach))
}
