package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ate-tooling/ate-tui/internal/demo"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// drive runs cmds synchronously, skipping ticks, and feeds results back.
func drive(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(500 * time.Millisecond):
		return // a tick or a blocking log wait
	}
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			drive(m, c)
		}
	case nil, tickMsg:
	default:
		_, next := m.Update(msg)
		drive(m, next)
	}
}

func demoModel(t *testing.T, w, h int) *Model {
	t.Helper()
	m := New(Options{
		Connect: func(context.Context, string) (Backend, ConnInfo, error) {
			return demo.New(), ConnInfo{Context: "demo", Cluster: "https://demo"}, nil
		},
		Refresh: time.Hour,
	})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	drive(m, m.connect())
	return m
}

func press(m *Model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "ctrl+z":
			msg = tea.KeyMsg{Type: tea.KeyCtrlZ}
		case "ctrl+d":
			msg = tea.KeyMsg{Type: tea.KeyCtrlD}
		case "ctrl+n":
			msg = tea.KeyMsg{Type: tea.KeyCtrlN}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := m.Update(msg)
		drive(m, cmd)
	}
}

func checkFrame(t *testing.T, m *Model, w, h int) string {
	t.Helper()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) != h {
		t.Errorf("%dx%d: frame has %d lines", w, h, len(lines))
	}
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("%dx%d: line %d is %d wide: %q", w, h, i, lw, l)
		}
	}
	return v
}

func TestFrameFitsTerminal(t *testing.T) {
	for _, sz := range [][2]int{{60, 20}, {80, 24}, {120, 40}, {200, 50}} {
		w, h := sz[0], sz[1]
		m := demoModel(t, w, h)
		m.opts.Write = true
		for _, step := range [][]string{
			nil,
			{"/", "crash", "enter"},
			{"enter"},
			{"esc", "esc", ":", "workers", "enter"},
			{"X"},
			{"esc", "y"},
			{"esc", "?"},
			{"esc", ":"},
			{"esc", ":", "actors", "enter", "space", "j", "space", "j", "space", "s"},
			{"tab"},
			{"esc", "ctrl+d"},
			{"wrong", "enter"},
			{"esc", "esc", ":", "workers", "enter", "D"},
			{"esc", ":", "journal", "enter"},
			{"esc", ":", "pulse", "enter"},
			{"j", "G"},
			{"esc", ":", "access", "enter"},
			{"esc", ":", "actors", "enter", "f"},
			{"esc", ":", "pf", "enter"},
			{"esc", "w"},
			{":", "ctx", "enter"},
			{"esc", "w"},
			{"esc", ":", "actors", "enter", "u"},
			{"j", "j"},
			{"enter"},
			{"u"},
			{"esc", ":", "templates", "enter", "u"},
			{"esc", ":", "workerpools", "enter", "u"},
			{"esc", ":", "actors", "enter", "e"},
			{"esc", "j", "E"},
			{"esc", ":", "workers", "enter", "E"},
			{"esc", ":", "actors", "enter", "l"},
			{"c", "w"},
			{"/", "listening", "enter", "n"},
			{"/", "typing"},
			{"esc", "esc", "esc"},
		} {
			press(m, step...)
			checkFrame(t, m, w, h)
		}
	}
}

func TestCommandPaletteRunsSuggestion(t *testing.T) {
	m := demoModel(t, 120, 40)
	press(m, ":", "wo", "enter")
	if m.cur.kind != kWorkers {
		t.Fatalf("kind = %s, want workers", m.cur.kind)
	}
	press(m, ":", "actors support", "enter")
	if m.cur.kind != kActors || m.atespace != "support" {
		t.Fatalf("got %s in %q, want actors in support", m.cur.kind, m.atespace)
	}
}

func TestGroupedRunesReplayAsKeys(t *testing.T) {
	m := demoModel(t, 120, 40)
	press(m, "/crash")
	if m.mode != modeFilter || m.table().filter != "crash" {
		t.Fatalf("mode %v filter %q", m.mode, m.table().filter)
	}
}

func TestCursorFollowsRowAcrossRefresh(t *testing.T) {
	m := demoModel(t, 120, 40)
	press(m, "j", "j", "j")
	id := m.selectedID()
	drive(m, m.fetch(m.cur))
	if got := m.selectedID(); got != id {
		t.Fatalf("selection moved from %s to %s", id, got)
	}
}

func TestFormatLogLine(t *testing.T) {
	mk := func(as, name, container string) []byte {
		c := ""
		if container != "" {
			c = fmt.Sprintf(`,"ate.actor.container.name":%q`, container)
		}
		return []byte(fmt.Sprintf(`{"message":"hi","labels":{"ate.atespace":%q,"ate.actor.name":%q%s}}`, as, name, c))
	}
	tests := []struct {
		name string
		line []byte
		want bool
	}{
		{"match", mk("a", "x", "agent"), true},
		{"lifecycle", mk("a", "x", ""), true},
		{"other actor", mk("a", "y", "agent"), false},
		{"same name other atespace", mk("b", "x", "agent"), false},
		{"sandbox container", mk("a", "x", "_sandbox"), false},
		{"not json", []byte("plain text"), false},
	}
	for _, tt := range tests {
		if _, ok := formatLogLine(tt.line, "a", "x"); ok != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, ok, tt.want)
		}
	}
}

func TestLayoutWidthsDropsThenRestores(t *testing.T) {
	// NAME(10) BIG(30) SMALL(3); avail fits NAME+SMALL but not BIG.
	w := layoutWidths([]int{10, 30, 3}, 20, []int{2, 1}, 6)
	if w[1] != 0 || w[2] != 3 {
		t.Fatalf("widths %v: want BIG hidden and SMALL restored", w)
	}
}

func TestFuzzyFilter(t *testing.T) {
	got := fuzzyFilter([]string{"workers", "workerpools", "actors", "tags"}, "wp")
	if len(got) != 1 || got[0] != "workerpools" {
		t.Fatalf("got %v", got)
	}
}

// BenchmarkFilterKeystroke is the cost of one filter keystroke on 10k actors.
func BenchmarkFilterKeystroke(b *testing.B) {
	now := time.Now()
	rows := make([]row, 10000)
	for i := range rows {
		rows[i] = actorRow(&ateapi.Actor{
			Metadata: &ateapi.ResourceMetadata{Atespace: "as", Name: fmt.Sprintf("actor-%05d", i),
				CreateTime: timestamppb.New(now), UpdateTime: timestamppb.New(now)},
			Status: &ateapi.ActorStatus{State: ateapi.ActorState(1 + i%5)},
		}, now)
	}
	ts := &tableState{}
	cols := lookupKind(string(kActors)).columns
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts.filter = "actor-0"[:1+i%7]
		ts.rebuild(rows, cols, "")
	}
}

// BenchmarkView is the cost of one frame with 10k actors loaded.
func BenchmarkView(b *testing.B) {
	m := New(Options{Connect: nil, Refresh: time.Hour})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	now := time.Now()
	rows := make([]row, 10000)
	for i := range rows {
		rows[i] = actorRow(&ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: "as", Name: fmt.Sprintf("actor-%05d", i)},
			Status: &ateapi.ActorStatus{State: ateapi.ActorState(1 + i%5)}}, now)
	}
	m.ds(m.cur).rows = rows
	m.ds(m.cur).fetchedAt = now
	m.table().rebuild(rows, m.def().columns, "")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}
