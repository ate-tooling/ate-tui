package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// markGlyph marks a row in the gutter.
const markGlyph = "▌"

func (m *Model) badge() string {
	if m.writable() {
		return stWrite.Render("[write]")
	}
	return stReadOnly.Render("[read-only]")
}

// overlay draws dlg centered over body, keeping the body on either side.
func overlay(body, dlg []string, width int) {
	if len(dlg) == 0 {
		return
	}
	dw := lipgloss.Width(dlg[0])
	x := max((width-dw)/2, 0)
	y := max((len(body)-len(dlg))/2, 0)
	for i, l := range dlg {
		if y+i >= len(body) {
			break
		}
		bg := body[y+i]
		body[y+i] = ansi.Truncate(bg, x, "") + "\x1b[0m" + l + ansi.TruncateLeft(bg, x+dw, "")
	}
}

// viewConfirm is the dialog for the pending action, at most maxH lines tall.
func (m *Model) viewConfirm(maxH int) []string {
	c := m.confirm
	a := c.act
	w := min(m.width-4, 72)
	inner := w - 4
	noun := string(c.key.kind)
	if len(c.targets) == 1 {
		noun = strings.TrimSuffix(noun, "s")
	}
	verb := strings.ToUpper(a.name[:1]) + a.name[1:]
	head := fmt.Sprintf("%s %d %s?", verb, len(c.targets), noun)
	if len(c.targets) == 1 {
		head = fmt.Sprintf("%s %s %s?", verb, noun, c.targets[0].id)
	}

	var tail []string
	if a.name == "suspend" {
		async, sync := "● async", "○ sync"
		if c.sync {
			async, sync = "○ async", "● sync"
		}
		tail = append(tail, "", stLabel.Render("durability  ")+stSuggestSel.Render(async)+"  "+stSuggest.Render(sync)+
			stDim.Render("   tab toggles"))
		if c.sync {
			tail = append(tail, stDim.Render("returns once the snapshot is in durable storage"))
		} else {
			tail = append(tail, stDim.Render("returns once the snapshot is on the node; upload continues"))
		}
	}
	keys := "y/⏎ confirm  ·  n/esc cancel"
	if a.typed {
		tail = append(tail, "", stCount.Render("Type ")+stStatusErr.Render(c.want())+stCount.Render(" to confirm:"),
			stInputPrompt.Render("> ")+stInput.Render(m.input)+stInput.Render("█"))
		if c.refused {
			tail = append(tail, stStatusErr.Render("does not match; nothing was deleted"))
		}
		keys = "⏎ confirm  ·  esc cancel"
	}
	tail = append(tail, "", stDim.Render(keys))

	// List as many objects as fit between the heading and the controls.
	room := max(maxH-2-2-len(tail), 1)
	var objs []string
	if len(c.targets) > 1 {
		for i, t := range c.targets {
			if i == room-1 && len(c.targets) > room {
				objs = append(objs, stDim.Render(fmt.Sprintf("  … and %d more", len(c.targets)-i)))
				break
			}
			objs = append(objs, "  "+fg(cBlue).Render(t.id))
		}
	}
	lines := append([]string{stCount.Bold(true).Render(head)}, "")
	if len(objs) > 0 {
		lines = append(lines, objs...)
	} else {
		lines = lines[:1]
	}
	lines = append(lines, tail...)

	border, title := fg(cPeach), stInputPrompt.Render(a.name)
	if a.typed {
		border, title = fg(cRed), fg(cRed).Bold(true).Render(a.name)
	}
	for i := range lines {
		lines[i] = fit(lines[i], inner)
	}
	h := min(len(lines), max(maxH-2, 1))
	return box(title, lines, w, h, border)
}
