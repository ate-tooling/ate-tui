package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// KubeContext is one kubeconfig context :ctx can switch to.
type KubeContext struct {
	Name    string
	Cluster string // the API server URL
	Current bool   // the kubeconfig's current-context
}

type contextsMsg struct {
	list []KubeContext
	err  error
}

// loadContexts reads the kubeconfig contexts off the UI goroutine.
func (m *Model) loadContexts() tea.Cmd {
	list := m.opts.Contexts
	if list == nil {
		return nil
	}
	return func() tea.Msg {
		cs, err := list()
		return contextsMsg{list: cs, err: err}
	}
}

func (m *Model) contextsLoaded(msg contextsMsg) {
	m.contexts, m.contextsErr = msg.list, msg.err
	if m.pager != nil && m.pager.contexts {
		m.pager.loading = false
		m.drawContexts()
	}
}

// openContexts lists the kubeconfig contexts (:ctx); ⏎ switches.
func (m *Model) openContexts() tea.Cmd {
	p := m.newPager(stTitleKind.Render("contexts"))
	p.contexts = true
	p.cursor = 0
	for i, c := range m.contexts {
		if c.Name == m.info.Context {
			p.cursor = i
		}
	}
	m.drawContexts()
	return m.loadContexts()
}

func (m *Model) drawContexts() {
	p := m.pager
	switch {
	case m.contextsErr != nil:
		p.lines = []string{stStatusErr.Render("kubeconfig: " + m.contextsErr.Error())}
		return
	case m.contexts == nil:
		p.lines = []string{stDim.Render("loading…")}
		return
	}
	p.cursor = min(p.cursor, max(len(m.contexts)-1, 0))
	w := len("NAME")
	for _, c := range m.contexts {
		w = max(w, min(len(c.Name), maxColWidth))
	}
	line := func(mark, name, cluster string) string {
		return fmt.Sprintf(" %-1s %-*s  %s", mark, w, name, cluster)
	}
	lines := []string{stColHeader.Render(line("", "NAME", "CLUSTER"))}
	for i, c := range m.contexts {
		mark, lv := "", lvNormal
		if c.Name == m.info.Context {
			mark, lv = "●", lvProgress
		}
		l := line(mark, c.Name, c.Cluster)
		if c.Current {
			l += stDim.Render("  (kubeconfig current)")
		}
		if i == p.cursor {
			lines = append(lines, stSelected.Render(fit(ansiStripLine(l), m.width-4)))
		} else {
			lines = append(lines, rowStyles[lv].Render(l))
		}
	}
	p.lines = lines
	listFollow(p, m.pagerHeight())
}

// listFollow keeps a list pager's cursor row on screen; row 0 is the
// column header.
func listFollow(p *pager, h int) {
	row := p.cursor + 1
	if row < p.offset+1 {
		p.offset = max(row-1, 0)
	}
	if row >= p.offset+h {
		p.offset = row - h + 1
	}
	p.scroll(0, h)
}

// listMove moves a list cursor for the usual keys and reports whether key
// was one of them.
func listMove(key string, cursor *int, n, h int) bool {
	d := 0
	switch key {
	case "j", "down":
		d = 1
	case "k", "up":
		d = -1
	case "g", "home":
		d = -n
	case "G", "end":
		d = n
	case "ctrl+f", "pgdown":
		d = h
	case "ctrl+b", "pgup":
		d = -h
	default:
		return false
	}
	if n > 0 {
		*cursor = min(max(*cursor+d, 0), n-1)
	}
	return true
}

func (m *Model) keyContexts(key string) (bool, tea.Cmd) {
	p := m.pager
	if listMove(key, &p.cursor, len(m.contexts), m.pagerHeight()) {
		m.drawContexts()
		return true, nil
	}
	if key == "enter" && p.cursor < len(m.contexts) {
		return true, m.switchContext(m.contexts[p.cursor].Name)
	}
	return false, nil
}

// switchContext drops everything learned from the current cluster and
// connects to name. It waits for writes and proxy starts in flight, so
// their results are journaled against the context they ran on.
func (m *Model) switchContext(name string) tea.Cmd {
	switch {
	case m.opts.Contexts == nil:
		m.setStatus("this backend has no kube contexts", true)
		return nil
	case len(m.batches) > 0 || m.fwdPending > 0:
		m.setStatus("wait for running actions and proxy starts to finish before switching context", true)
		return nil
	case name == m.info.Context && m.backend != nil:
		m.closePager()
		m.mode = modeTable
		m.setStatus("already on "+name, false)
		return nil
	}
	known := m.contexts == nil
	for _, c := range m.contexts {
		known = known || c.Name == name
	}
	if !known {
		m.setStatus("no context "+name+" in the kubeconfig", true)
		return nil
	}

	var cmds []tea.Cmd
	for _, f := range m.forwards {
		cmds = append(cmds, m.record(journalEntry{Time: time.Now(), Context: m.info.Context, Action: "proxy stop",
			Mode: fwdSpec(f), Object: f.Actor, Result: "ok"}))
	}
	old, fws := m.backend, m.forwards
	cmds = append(cmds, func() tea.Msg {
		for _, f := range fws {
			f.Stop()
		}
		if c, ok := old.(interface{ Close() }); ok {
			c.Close()
		}
		return nil
	})

	m.closePager()
	m.mode, m.confirm, m.fwd = modeTable, nil, nil
	m.kubeCtx = name
	m.backend, m.info, m.connErr = nil, ConnInfo{Context: name}, nil
	m.forwards = nil
	m.data, m.tables = map[viewKey]*dataset{}, map[viewKey]*tableState{}
	m.timeline, m.lastState = nil, map[string]string{}
	m.want, m.worker, m.atespace = wantRow{}, "", ""
	k := m.cur.kind
	if workerScoped(k) {
		k = kActors
	}
	m.cur = m.keyFor(k)
	m.history, m.histIdx = []viewKey{m.cur}, 0
	m.setStatus("Switching to context "+name+"…", false)
	return tea.Batch(append(cmds, m.connect())...)
}

// contextNames completes :ctx.
func (m *Model) contextNames(head string) []string {
	out := make([]string, 0, len(m.contexts))
	for _, c := range m.contexts {
		out = append(out, head+" "+c.Name)
	}
	return out
}

func ansiStripLine(s string) string { return strings.TrimRight(pagerText([]string{s}), " ") }
