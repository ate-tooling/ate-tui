package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	wheelStep = 3
	// doubleClick is how soon a second click on the selected row opens it.
	doubleClick = 500 * time.Millisecond
)

// handleMouse scrolls with the wheel and selects with a click; a double
// click acts like ⏎. Dialogs and text entry ignore the mouse.
func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Action != tea.MouseActionPress {
		return nil
	}
	switch m.mode {
	case modeTable:
		return m.mouseTable(msg)
	case modePager:
		if m.pager != nil {
			return m.mousePager(msg)
		}
	}
	return nil
}

func (m *Model) mouseTable(msg tea.MouseMsg) tea.Cmd {
	t := m.table()
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		d := wheelStep
		if msg.Button == tea.MouseButtonWheelUp {
			d = -wheelStep
		}
		t.cursor += d
		t.clamp(m.tableHeight())
	case tea.MouseButtonLeft:
		// Rows start under the header box, the table's top border and its
		// column header.
		i := t.offset + msg.Y - headerHeight - 2
		if msg.Y < headerHeight+2 || i < t.offset || i >= min(len(t.visible), t.offset+m.tableHeight()) {
			return nil
		}
		return m.click(i, &t.cursor, func() tea.Cmd {
			if r := m.selected(); r != nil {
				return m.keyRow("enter", r)
			}
			return nil
		})
	}
	return nil
}

func (m *Model) mousePager(msg tea.MouseMsg) tea.Cmd {
	p := m.pager
	list := p.items != nil || p.forwards || p.contexts
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		key := "j"
		if msg.Button == tea.MouseButtonWheelUp {
			key = "k"
		}
		if list {
			return m.keyPager(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		}
		h := m.pagerHeight()
		if p.raw {
			h = m.pulseHeight()
		}
		d := wheelStep
		if key == "k" {
			d = -wheelStep
		}
		p.scroll(d, h)
	case tea.MouseButtonLeft:
		if !list || msg.Y <= headerHeight {
			return nil
		}
		// Rows start under the box's top border; line 0 of a list is its
		// column header. The bottom border and footer are not rows.
		r := msg.Y - headerHeight - 1
		if r >= m.pagerHeight() {
			return nil
		}
		i := p.offset + r - 1
		n := len(p.items)
		switch {
		case p.forwards:
			n = len(m.forwards)
		case p.contexts:
			n = len(m.contexts)
		}
		if i < 0 || i >= n {
			return nil
		}
		redraw := func() {
			switch {
			case p.forwards:
				m.drawForwards()
			case p.contexts:
				m.drawContexts()
			default:
				m.drawRelations()
			}
		}
		cmd := m.click(i, &p.cursor, func() tea.Cmd {
			return m.keyPager(tea.KeyMsg{Type: tea.KeyEnter})
		})
		if m.pager == p {
			redraw()
		}
		return cmd
	}
	return nil
}

// click moves the cursor to i, or opens the row when it is already there
// and was clicked a moment ago.
func (m *Model) click(i int, cursor *int, open func() tea.Cmd) tea.Cmd {
	now := time.Now()
	again := *cursor == i && now.Sub(m.lastClick) < doubleClick
	*cursor = i
	if again {
		m.lastClick = time.Time{}
		return open()
	}
	m.lastClick = now
	return nil
}
