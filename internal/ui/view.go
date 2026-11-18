package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	headerHeight = 7
	footerHeight = 2
	logoWidth    = 18
)

// The header art: an actor asleep in its sandbox.
var logo = []string{
	"  ┌───────────┐",
	"  │  ╭─────╮  │",
	"  │  │ ◉ ◉ │  │",
	"  │  │  ▿  │  │",
	"  │  ╰─────╯  │",
	"  └───────────┘",
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range m.viewHeader() {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if m.mode == modeCommand {
		for _, l := range m.viewCommand() {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	var body []string
	if m.mode == modePager && m.pager != nil {
		body = m.viewPager()
	} else {
		body = m.viewTable()
	}
	if m.mode == modeConfirm && m.confirm != nil {
		overlay(body, m.viewConfirm(len(body)), m.width)
	}
	for _, l := range body {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString(strings.Join(m.viewFooter(), "\n"))
	return b.String()
}

// fit pads or truncates a styled string to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	sw := lipgloss.Width(s)
	if sw > w {
		return ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-sw)
}

// box draws a rounded border with a title in the top edge, like sofka.
func box(title string, lines []string, width, height int, border lipgloss.Style) []string {
	inner := width - 2
	out := make([]string, 0, height+2)
	tw := lipgloss.Width(title)
	rest := max(inner-tw-2, 0)
	out = append(out, border.Render("╭ ")+title+border.Render(" "+strings.Repeat("─", rest)+"╮"))
	side := border.Render("│")
	for i := 0; i < height; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out = append(out, side+" "+fit(l, inner-2)+" "+side)
	}
	out = append(out, border.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return out
}

func (m *Model) viewHeader() []string {
	showLogo := m.width >= 90
	w := m.width
	if showLogo {
		w -= logoWidth
	}
	scope := m.atespace
	if scope == "" {
		scope = "<all>"
	}
	if m.cur.kind == kAssignments {
		scope = "worker " + m.worker
	}
	ctx := m.info.Context
	if ctx == "" {
		ctx = m.opts.ConnLabel
	}
	cluster := m.info.Cluster
	switch {
	case m.connecting:
		cluster = stDim.Render("connecting…")
	case m.connErr != nil:
		cluster = stStatusErr.Render("unreachable")
	}
	t := m.table()
	total := 0
	if d := m.data[m.cur]; d != nil {
		total = len(d.rows)
	}
	info := []string{
		stLabel.Render("Context:   ") + stContext.Render(ctx) + "  " + m.badge(),
		stLabel.Render("Cluster:   ") + stCluster.Render(cluster),
		stLabel.Render("Atespace:  ") + stAtespace.Render(scope),
		stLabel.Render("Resource:  ") + stResource.Render(string(m.cur.kind)),
		stLabel.Render("Count:     ") + stCount.Render(fmtCount(len(t.visible), total)),
	}

	// Key hints fill columns of five rows to the right of the info block.
	const infoW, hintW = 44, 18
	hints := m.def().hints
	cols := max((w-4-infoW)/hintW, 0)
	lines := make([]string, 5)
	for i := range lines {
		lines[i] = fit(info[i], infoW)
		for c := 0; c < cols; c++ {
			idx := c*5 + i
			if idx < len(hints) {
				h := hints[idx]
				lines[i] += fit(stHintKey.Render(h.key)+" "+stHintDesc.Render(h.desc), hintW)
			}
		}
	}
	out := box(stBrand.Render("ate-tui"), lines, w, 5, stHeaderBorder)
	if showLogo {
		for i := range out {
			l := ""
			if i < len(logo) {
				l = colorLogo(logo[i])
			} else if i == len(logo) {
				l = stVersion.Render(fmt.Sprintf("%*s", logoWidth-1, "ate-tui "+m.opts.Version))
			}
			out[i] += fit(l, logoWidth)
		}
	}
	return out
}

func colorLogo(l string) string {
	var b strings.Builder
	for _, r := range l {
		switch r {
		case '◉':
			b.WriteString(stLogoEye.Render(string(r)))
		default:
			b.WriteString(stLogo.Render(string(r)))
		}
	}
	return b.String()
}

func (m *Model) viewCommand() []string {
	line := stInputPrompt.Render(":") + stInput.Render(m.input)
	if len(m.suggestions) > 0 {
		s := m.suggestions[m.suggestIdx]
		if strings.HasPrefix(s, m.input) {
			line += stGhost.Render(s[len(m.input):])
		}
		var alts []string
		for i, sg := range m.suggestions {
			if i >= 8 {
				break
			}
			if i == m.suggestIdx {
				alts = append(alts, stSuggestSel.Render(sg))
			} else {
				alts = append(alts, stSuggest.Render(sg))
			}
		}
		line = fit(line, 32) + strings.Join(alts, stDim.Render("  "))
	} else {
		line += stInput.Render("█")
	}
	return box(stInputPrompt.Render("command"), []string{line}, m.width, 1, fg(cPeach))
}

func (m *Model) viewTable() []string {
	def := m.def()
	t := m.table()
	d := m.data[m.cur]
	h := m.tableHeight()
	inner := m.width - 4

	title := stTitleKind.Render(string(m.cur.kind))
	if def.scoped || m.cur.kind == kAssignments {
		scope := m.cur.scope
		if scope == "" {
			scope = "all"
		}
		title += stDim.Render("(") + stAtespace.Render(scope) + stDim.Render(")")
	}
	total := 0
	if d != nil {
		total = len(d.rows)
	}
	title += " " + stTitleCount.Render("["+fmtCount(len(t.visible), total)+"]")
	if t.filter != "" || m.mode == modeFilter {
		f := "/" + t.filter
		if m.mode == modeFilter {
			f = "/" + m.input + "█"
		}
		title += " " + stTitleFilter.Render(f)
	}
	if t.faults {
		title += " " + stStatusErr.Render("faults")
	}
	if n := len(t.marked); n > 0 {
		title += " " + stMark.Render(fmt.Sprintf("marked %d", n))
	}

	natural := t.widths
	if len(natural) != len(def.columns) {
		natural = headerWidths(def.columns)
	}
	widths := layoutWidths(natural, inner-2, def.drop)
	cols := make([]string, len(def.columns))
	copy(cols, def.columns)
	if t.sortCol < len(cols) {
		arrow := "↑"
		if t.sortDesc {
			arrow = "↓"
		}
		cols[t.sortCol] += arrow
	}
	lines := []string{stColHeader.Render(renderCells(cols, widths, " "))}

	switch {
	case d == nil || (d.fetchedAt.IsZero() && d.err == nil):
		lines = append(lines, "", stDim.Render(centered("loading "+string(m.cur.kind)+"…", inner)))
	case d.err != nil && len(d.rows) == 0:
		lines = append(lines, "", stStatusErr.Render(centered(errString(d.err), inner)))
	case len(t.visible) == 0:
		lines = append(lines, "", stDim.Render(centered("no "+string(m.cur.kind)+" match", inner)))
	default:
		end := min(t.offset+h, len(t.visible))
		for i := t.offset; i < end; i++ {
			r := &d.rows[t.visible[i]]
			marked := len(t.marked) > 0 && t.marked[r.id]
			gutter := " "
			if marked {
				gutter = markGlyph
			}
			text := renderCells(r.cells, widths, gutter)
			switch {
			case i == t.cursor:
				lines = append(lines, stSelected.Render(fit(text, inner)))
			case marked:
				lines = append(lines, stMark.Render(markGlyph)+rowStyles[r.level].Render(text[len(markGlyph):]))
			default:
				lines = append(lines, rowStyles[r.level].Render(text))
			}
		}
	}
	return box(title, lines, m.width, h+1, stTableBorder)
}

func headerWidths(cols []string) []int {
	w := make([]int, len(cols))
	for i, c := range cols {
		w[i] = len(c) + 1
	}
	return w
}

func centered(s string, w int) string {
	pad := max((w-lipgloss.Width(s))/2, 0)
	return strings.Repeat(" ", pad) + s
}

func (m *Model) viewPager() []string {
	p := m.pager
	h := m.pagerHeight()
	end := min(p.offset+h, len(p.lines))
	lines := p.lines[p.offset:end]
	title := p.title
	if len(p.lines) > h {
		title += stDim.Render(fmt.Sprintf(" %d-%d/%d", p.offset+1, end, len(p.lines)))
	}
	if m.logs != nil {
		if p.follow {
			title += " " + stLive.Render("following")
		} else {
			title += " " + stPaused.Render("paused")
		}
	}
	return box(title, lines, m.width, h, stTableBorder)
}

func (m *Model) viewFooter() []string {
	var hints []hint
	switch m.mode {
	case modePager:
		hints = []hint{{"esc", "back"}, {"j/k", "scroll"}, {"g/G", "top/bottom"}}
		if m.logs != nil {
			hints = append(hints, hint{"f", "follow"})
		}
		hints = append(hints, hint{":", "resource"}, hint{"?", "help"})
	case modeFilter:
		hints = []hint{{"⏎", "keep"}, {"esc", "clear"}, {"!term", "exclude"}}
	case modeCommand:
		hints = []hint{{"tab", "complete"}, {"↑↓", "choose"}, {"⏎", "run"}, {"esc", "cancel"}}
	case modeConfirm:
		if m.confirm != nil && m.confirm.act.typed {
			hints = []hint{{"⏎", "confirm"}, {"esc", "cancel"}}
		} else {
			hints = []hint{{"y/⏎", "confirm"}, {"n/esc", "cancel"}}
		}
	default:
		hints = []hint{{":", "resource"}, {"/", "filter"}, {"S", "sort"}, {"I", "invert"},
			{"^z", "faults"}, {"[ ]", "history"}, {"0", "all-atespaces"}, {"␣", "mark"}, {"?", "help"}}
	}
	var parts []string
	for _, h := range hints {
		sep := ":"
		if h.key == ":" || h.key == "/" {
			sep = ""
		}
		parts = append(parts, stFooterHint.Render(h.key+sep+h.desc))
	}
	line1 := " " + strings.Join(parts, "  ")

	status := stStatus.Render(m.status)
	if m.statusErr {
		status = stStatusErr.Render(m.status)
	}
	live := m.liveIndicator()
	lw := lipgloss.Width(live)
	line2 := " " + fit(status, m.width-lw-2) + live
	return []string{fit(line1, m.width), line2}
}

func (m *Model) liveIndicator() string {
	switch {
	case m.connecting:
		return stPaused.Render("○ connecting")
	case m.connErr != nil:
		return stDead.Render("● offline")
	}
	d := m.data[m.cur]
	if d == nil || d.fetchedAt.IsZero() {
		return stPaused.Render("○ loading")
	}
	if d.err != nil || time.Since(d.fetchedAt) > 3*m.opts.Refresh+15*time.Second {
		return stPaused.Render("● stale")
	}
	return stLive.Render("● live")
}
