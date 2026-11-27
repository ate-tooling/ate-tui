package ui

import (
	"fmt"
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// deliver feeds a fetch result for k as if its list had just returned.
func deliver(m *Model, k viewKey, rows []row) {
	d := m.ds(k)
	m.gen++
	d.gen, d.loading = m.gen, true
	m.Update(fetchedMsg{key: k, gen: d.gen, rows: rows, at: time.Now()})
}

func TestBackgroundFetchShrinksHiddenTable(t *testing.T) {
	m := demoModel(t, 120, 40)
	actors := m.keyFor(kActors)
	if m.cur != actors {
		t.Fatalf("start view %v", m.cur)
	}
	press(m, "G")
	press(m, ":", "workers", "enter")
	if m.cur.kind != kWorkers {
		t.Fatalf("view %v", m.cur)
	}
	old := m.data[actors].rows
	if len(old) < 2 {
		t.Fatalf("demo has %d actors", len(old))
	}
	keepRow := old[0]
	deliver(m, actors, []row{keepRow})
	press(m, "[")
	if m.cur != actors {
		t.Fatalf("back went to %v", m.cur)
	}
	if got := m.selectedID(); got != keepRow.id {
		t.Errorf("selected %q, want %q", got, keepRow.id)
	}
}

func TestBackgroundFetchKeepsHiddenSelection(t *testing.T) {
	m := demoModel(t, 120, 40)
	actors := m.keyFor(kActors)
	press(m, "j", "j")
	want := m.selectedID()
	press(m, ":", "workers", "enter")
	rows := slices.Clone(m.data[actors].rows)
	slices.Reverse(rows)
	deliver(m, actors, rows)
	press(m, "[")
	if got := m.selectedID(); got != want {
		t.Errorf("selected %q, want %q", got, want)
	}
}

func TestSelectedIgnoresStaleIndex(t *testing.T) {
	m := demoModel(t, 120, 40)
	press(m, "G")
	d := m.data[m.cur]
	d.rows = d.rows[:1]
	if r := m.selected(); r != nil {
		t.Errorf("selected %q from a stale index", r.id)
	}
}

func tlRow(id, state string) row {
	return newRow(id, nil, 0, []string{"ns", id, state, "tpl", "w1"}, nil)
}

func TestTimelineIgnoresLateObservations(t *testing.T) {
	m := demoModel(t, 120, 40)
	m.timeline, m.lastState, m.lastSeen = nil, map[string]string{}, map[string]time.Time{}
	t0 := time.Now()
	m.recordTimeline([]row{tlRow("a", "RUNNING")}, t0)
	m.recordTimeline([]row{tlRow("a", "SUSPENDED")}, t0.Add(2*time.Second))
	// A fetch started before the last one returns afterwards.
	m.recordTimeline([]row{tlRow("a", "RUNNING")}, t0.Add(time.Second))
	m.recordTimeline([]row{tlRow("a", "SUSPENDED")}, t0.Add(3*time.Second))
	var got []string
	for _, e := range m.timeline {
		got = append(got, e.from+">"+e.to)
	}
	if want := []string{">RUNNING", "RUNNING>SUSPENDED"}; !slices.Equal(got, want) {
		t.Errorf("timeline %v, want %v", got, want)
	}
}

func TestWideOffClampsEveryTable(t *testing.T) {
	m := demoModel(t, 200, 40)
	press(m, "w")
	if !m.wide {
		t.Fatal("w did not turn wide on")
	}
	actors := m.keyFor(kActors)
	at := m.tableFor(actors)
	at.sortCol = len(m.columns(kActors)) - 1
	press(m, ":", "workers", "enter")
	press(m, "w")
	if m.wide {
		t.Fatal("w did not turn wide off")
	}
	if n := len(m.columns(kActors)); at.sortCol >= n {
		t.Errorf("actors sort column %d with %d columns", at.sortCol, n)
	}
}

func TestResizeKeepsPagerFollowing(t *testing.T) {
	m := demoModel(t, 120, 40)
	p := m.newPager("test")
	p.loading, p.follow = false, true
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprint(i)
	}
	p.setLines(lines, m.pagerHeight())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if want := len(lines) - m.pagerHeight(); p.offset != want {
		t.Errorf("following offset %d after shrink, want %d", p.offset, want)
	}
	p.follow = false
	p.scroll(-1000, m.pagerHeight())
	p.offset = len(lines) - m.pagerHeight()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
	if max := len(lines) - m.pagerHeight(); p.offset > max {
		t.Errorf("offset %d past the end %d after growing", p.offset, max)
	}
}
