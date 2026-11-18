package ui

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
)

// tabKinds is the Tab / Shift-Tab cycle.
var tabKinds = []kind{kActors, kWorkers, kTemplates, kWorkerPools, kAtespaces, kTags}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		return m.quit()
	}
	// A confirm dialog takes every key. Grouped runes are not replayed, so a
	// paste cannot confirm a write.
	if m.mode == modeConfirm {
		return m.keyConfirm(msg)
	}
	// Bubble Tea groups runes that arrive together, as with a paste or fast
	// typing. Outside text entry, replay them one key at a time so "/crash"
	// opens the filter with "crash".
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && m.mode != modeFilter && m.mode != modeCommand {
		var cmds []tea.Cmd
		for i, r := range msg.Runes {
			if m.mode == modeFilter || m.mode == modeCommand {
				cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: msg.Runes[i:], Paste: msg.Paste}))
				break
			}
			cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
			if m.mode == modeConfirm {
				break // drop the rest: "sy" typed fast must not confirm
			}
		}
		return tea.Batch(cmds...)
	}
	switch m.mode {
	case modeFilter:
		return m.keyFilter(msg)
	case modeCommand:
		return m.keyCommand(msg)
	case modePager:
		return m.keyPager(msg)
	}
	return m.keyTable(msg)
}

func (m *Model) quit() tea.Cmd {
	m.closePager()
	return tea.Quit
}

func (m *Model) keyTable(msg tea.KeyMsg) tea.Cmd {
	t := m.table()
	h := m.tableHeight()
	move := func(n int) tea.Cmd {
		t.cursor += n
		t.clamp(h)
		return nil
	}
	switch msg.String() {
	case "q":
		return m.quit()
	case "j", "down":
		return move(1)
	case "k", "up":
		return move(-1)
	case "g", "home":
		return move(-len(t.visible))
	case "G", "end":
		return move(len(t.visible))
	case "ctrl+f", "pgdown":
		return move(h)
	case "ctrl+b", "pgup":
		return move(-h)
	case "/":
		m.mode, m.input = modeFilter, t.filter
		return nil
	case ":":
		m.mode, m.input = modeCommand, ""
		m.updateSuggestions()
		return nil
	case "?":
		m.openHelp()
		return nil
	case "esc":
		if t.filter != "" || t.faults {
			t.filter, t.faults = "", false
			m.rebuild()
			return nil
		}
		return m.historyMove(-1)
	case "[":
		return m.historyMove(-1)
	case "]":
		return m.historyMove(1)
	case "tab", "shift+tab":
		step := 1
		if msg.String() == "shift+tab" {
			step = len(tabKinds) - 1
		}
		i := 0
		for j, k := range tabKinds {
			if k == m.cur.kind {
				i = j
			}
		}
		return m.gotoKind(tabKinds[(i+step)%len(tabKinds)], "")
	case "0":
		return m.setAtespace("")
	case "S":
		t.sortCol = (t.sortCol + 1) % len(m.def().columns)
		m.setStatus("Sort by "+m.def().columns[t.sortCol], false)
		m.rebuild()
		return nil
	case "I":
		t.sortDesc = !t.sortDesc
		m.rebuild()
		return nil
	case "ctrl+z":
		t.faults = !t.faults
		m.rebuild()
		if t.faults {
			m.setStatus("Showing faults only", false)
		} else {
			m.setStatus("Viewing "+string(m.cur.kind), false)
		}
		return nil
	case " ":
		m.toggleMark()
		return nil
	case "r":
		if m.backend == nil && !m.connecting {
			m.setStatus("Reconnecting…", false)
			return m.connect()
		}
		return m.fetch(m.cur)
	}

	if a := lookupAction(m.cur.kind, msg.String()); a != nil {
		return m.startAction(a)
	}
	r := m.selected()
	if r == nil {
		return nil
	}
	return m.keyRow(msg.String(), r)
}

// keyRow handles keys that act on the selected row.
func (m *Model) keyRow(key string, r *row) tea.Cmd {
	switch key {
	case "y":
		return m.openYAML(m.cur, r.id)
	case "X":
		return m.openExplain(m.cur, r.id)
	case "u":
		return m.openRelations(r)
	}
	switch o := r.obj.(type) {
	case *ateapi.Actor:
		switch key {
		case "enter":
			return m.openExplain(m.cur, r.id)
		case "l":
			return m.openLogs(o)
		case "e":
			return m.openEgress(o)
		case "E":
			wa := o.GetStatus().GetWorkerAssignment()
			if wa.GetWorkerPod() == "" {
				m.setStatus("actor is "+explain.ActorState(o.GetStatus().GetState())+" and not on a worker pod", true)
				return nil
			}
			return m.openEvents(wa.GetWorkerNamespace(), wa.GetWorkerPod())
		case "T":
			m.openTimeline(r.id)
			return nil
		case "o":
			if w := o.GetStatus().GetWorkerAssignment().GetWorker().GetName(); w != "" {
				return m.gotoKind(kWorkers, w)
			}
			m.setStatus("Actor has no worker", true)
		case "t":
			m.atespace = o.GetActorTemplate().GetAtespace()
			return m.gotoKind(kTemplates, o.GetActorTemplate().GetName())
		}
	case *ateapi.Worker:
		switch key {
		case "enter":
			m.worker = o.GetMetadata().GetName()
			return m.gotoKind(kAssignments, "")
		case "p":
			return m.gotoKind(kWorkerPools, o.GetWorkerPool())
		case "E":
			if o.GetWorkerPod() == "" {
				m.setStatus("worker has no pod", true)
				return nil
			}
			return m.openEvents(o.GetWorkerNamespace(), o.GetWorkerPod())
		}
	case *ateapi.ActorTemplate:
		if key == "enter" {
			m.atespace = o.GetMetadata().GetAtespace()
			return m.gotoKind(kActors, o.GetMetadata().GetName())
		}
	case *ateapi.Atespace:
		if key == "enter" {
			return m.setAtespace(o.GetMetadata().GetName())
		}
	case *infrav1.WorkerPool:
		if key == "enter" {
			return m.gotoKind(kWorkers, o.Name)
		}
	case *ateapi.ActorAssignment:
		if key == "enter" {
			m.atespace = o.GetActor().GetAtespace()
			return m.gotoKind(kActors, o.GetActor().GetName())
		}
	case *ateapi.Tag:
		if key == "enter" {
			return m.openYAML(m.cur, r.id)
		}
	}
	return nil
}

func (m *Model) rebuild() {
	t := m.table()
	if d := m.data[m.cur]; d != nil {
		t.rebuild(d.rows, m.def().columns, m.selectedID())
		t.clamp(m.tableHeight())
	}
}

func (m *Model) historyMove(step int) tea.Cmd {
	i := m.histIdx + step
	if i < 0 || i >= len(m.history) {
		return nil
	}
	m.histIdx = i
	k := m.history[i]
	if k.kind == kAssignments {
		m.worker = k.scope
	} else if lookupKind(string(k.kind)).scoped {
		m.atespace = k.scope
	}
	return m.setView(k, false)
}

// editInput applies a text-editing key and reports whether it consumed it.
func (m *Model) editInput(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyRunes:
		m.input += string(msg.Runes)
	case tea.KeySpace:
		m.input += " "
	case tea.KeyBackspace:
		if m.input != "" {
			_, n := utf8.DecodeLastRuneInString(m.input)
			m.input = m.input[:len(m.input)-n]
		}
	case tea.KeyCtrlU:
		m.input = ""
	case tea.KeyCtrlW:
		s := strings.TrimRight(m.input, " ")
		if i := strings.LastIndex(s, " "); i >= 0 {
			m.input = s[:i+1]
		} else {
			m.input = ""
		}
	default:
		return false
	}
	return true
}

func (m *Model) keyFilter(msg tea.KeyMsg) tea.Cmd {
	t := m.table()
	switch msg.Type {
	case tea.KeyEnter:
		m.mode = modeTable
		return nil
	case tea.KeyEsc:
		m.mode, t.filter = modeTable, ""
		m.rebuild()
		return nil
	case tea.KeyUp, tea.KeyDown:
		m.mode = modeTable
		return m.keyTable(msg)
	}
	if m.editInput(msg) {
		t.filter = m.input
		m.rebuild()
	}
	return nil
}

func (m *Model) keyCommand(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeTable
		return nil
	case tea.KeyEnter:
		in := strings.TrimSpace(m.input)
		// Run the highlighted suggestion unless the input is already a
		// complete command, so ":wo" + Enter opens workers.
		if len(m.suggestions) > 0 && !m.isCommand(in) {
			in = m.suggestions[m.suggestIdx]
		}
		m.mode = modeTable
		return m.runCommand(in)
	case tea.KeyTab, tea.KeyDown, tea.KeyCtrlN:
		if len(m.suggestions) > 0 {
			if msg.Type == tea.KeyTab {
				m.input = m.suggestions[m.suggestIdx] + " "
				m.updateSuggestions()
				return nil
			}
			m.suggestIdx = (m.suggestIdx + 1) % len(m.suggestions)
		}
		return nil
	case tea.KeyUp, tea.KeyCtrlP:
		if len(m.suggestions) > 0 {
			m.suggestIdx = (m.suggestIdx + len(m.suggestions) - 1) % len(m.suggestions)
		}
		return nil
	}
	if m.editInput(msg) {
		m.updateSuggestions()
	}
	return nil
}

var builtinCommands = []string{"quit", "help", "pulse", "access", "timeline", "journal", "all"}

// updateSuggestions completes the word under the cursor: a kind or command
// first, then an atespace name.
func (m *Model) updateSuggestions() {
	m.suggestIdx = 0
	fields := strings.Fields(m.input)
	trailing := strings.HasSuffix(m.input, " ")
	var pool []string
	prefix := ""
	switch {
	case len(fields) == 0 || (len(fields) == 1 && !trailing):
		for _, k := range kinds {
			pool = append(pool, string(k.kind))
		}
		pool = append(pool, builtinCommands...)
		if len(fields) == 1 {
			prefix = fields[0]
		}
	default:
		head := fields[0]
		k := lookupKind(head)
		if k == nil || !(k.scoped || k.kind == kAtespaces) {
			m.suggestions = nil
			return
		}
		if d := m.data[viewKey{kAtespaces, ""}]; d != nil {
			for _, r := range d.rows {
				pool = append(pool, head+" "+r.id)
			}
		}
		prefix = strings.Join(fields, " ")
		if trailing {
			prefix += " "
		}
	}
	m.suggestions = fuzzyFilter(pool, strings.ToLower(prefix))
}

// fuzzyFilter keeps candidates that contain query as a subsequence, prefix
// matches first.
func fuzzyFilter(pool []string, query string) []string {
	type scored struct {
		s     string
		score int
	}
	var out []scored
	for _, c := range pool {
		lc := strings.ToLower(c)
		switch {
		case strings.HasPrefix(lc, query):
			out = append(out, scored{c, 0})
		case strings.Contains(lc, query):
			out = append(out, scored{c, 1})
		case subsequence(lc, query):
			out = append(out, scored{c, 2})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score < out[j].score })
	res := make([]string, 0, len(out))
	for _, o := range out {
		res = append(res, o.s)
	}
	return res
}

func subsequence(s, q string) bool {
	i := 0
	for _, r := range s {
		if i < len(q) && byte(r) == q[i] {
			i++
		}
	}
	return i == len(q)
}

func (m *Model) isCommand(in string) bool {
	fields := strings.Fields(in)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "q", "q!", "quit", "exit", "help", "h", "pulse", "access", "timeline", "events", "journal", "all":
		return true
	}
	return lookupKind(fields[0]) != nil
}

func (m *Model) runCommand(in string) tea.Cmd {
	fields := strings.Fields(in)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case "q", "q!", "quit", "exit":
		return m.quit()
	case "help", "h":
		m.openHelp()
		return nil
	case "timeline":
		m.openTimeline("")
		return nil
	case "events":
		// Same as E on the selected actor or worker.
		if r := m.selected(); r != nil {
			return m.keyRow("E", r)
		}
		m.setStatus("select an actor or worker to see its pod's events", true)
		return nil
	case "journal":
		return m.openJournal()
	case "pulse":
		return m.openPulse()
	case "access":
		return m.openAccess()
	case "all":
		return m.setAtespace("")
	}
	k := lookupKind(fields[0])
	if k == nil {
		m.setStatus("unknown command "+fields[0], true)
		return nil
	}
	if k.kind == kAtespaces && len(fields) > 1 {
		return m.setAtespace(fields[1])
	}
	if len(fields) > 1 && k.scoped {
		a := fields[1]
		if a == "all" || a == "*" {
			a = ""
		}
		m.atespace = a
	}
	return m.gotoKind(k.kind, m.tableFor(m.keyFor(k.kind)).filter)
}

func (m *Model) keyPager(msg tea.KeyMsg) tea.Cmd {
	p := m.pager
	if m.logs != nil && m.keyLogs(msg) {
		return nil
	}
	if p.items != nil {
		if ok, cmd := m.keyRelations(msg.String()); ok {
			return cmd
		}
	}
	h := m.pagerHeight()
	if p.raw {
		h = m.pulseHeight()
	}
	switch msg.String() {
	case "esc", "q":
		m.closePager()
		m.mode = modeTable
		return nil
	case "j", "down":
		p.scroll(1, h)
	case "k", "up":
		p.scroll(-1, h)
	case "ctrl+f", "pgdown", " ":
		p.scroll(h, h)
	case "ctrl+b", "pgup":
		p.scroll(-h, h)
	case "g", "home":
		p.scroll(-len(p.lines), h)
	case "G", "end":
		p.scroll(len(p.lines), h)
	case "f":
		if m.logs != nil {
			p.follow = !p.follow
			if p.follow {
				p.scroll(len(p.lines), h)
			}
		}
	case ":":
		m.closePager()
		m.mode, m.input = modeCommand, ""
		m.updateSuggestions()
	case "?":
		m.openHelp()
	}
	return nil
}
