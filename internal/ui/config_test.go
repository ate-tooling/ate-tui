package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ate-tooling/ate-tui/internal/demo"
	"github.com/ate-tooling/ate-tui/internal/skin"
	tea "github.com/charmbracelet/bubbletea"
)

func TestParseKeymap(t *testing.T) {
	km, err := ParseKeymap(map[string]string{"suspend": "ctrl+s", "mark": "space", "explain": "X"})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"ctrl+s": "s", "j": "j", "X": "X"} {
		if got, ok := km.translate(in); !ok || got != want {
			t.Errorf("translate(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := km.translate("s"); ok {
		t.Error("s still suspends after suspend moved to ctrl+s")
	}
	if got := km.show("s"); got != "^s" {
		t.Errorf("show(s) = %q", got)
	}
	if got := km.show("[ ]"); got != "[ ]" {
		t.Errorf("show([ ]) = %q", got)
	}

	for cfg, want := range map[string]map[string]string{
		"unknown action": {"suspnd": "x"},
		"reserved":       {"quit": "enter"},
		"bound to both":  {"suspend": "S"}, // S sorts
		"not a key":      {"quit": "quit"},
	} {
		if _, err := ParseKeymap(want); err == nil || !strings.Contains(err.Error(), cfg) {
			t.Errorf("%v: err %v, want %q", want, err, cfg)
		}
	}
	// Moving sort away frees S for suspend.
	if _, err := ParseKeymap(map[string]string{"suspend": "S", "sort": "ctrl+o"}); err != nil {
		t.Errorf("swap: %v", err)
	}
}

func TestReboundKeysDriveTheTable(t *testing.T) {
	m := demoModel(t, 160, 40)
	km, err := ParseKeymap(map[string]string{"yaml": "Y", "down": "J"})
	if err != nil {
		t.Fatal(err)
	}
	m.keys = km
	before := m.table().cursor
	press(m, "J")
	if m.table().cursor != before+1 {
		t.Errorf("J did not move down")
	}
	press(m, "y")
	if m.mode != modeTable {
		t.Error("y still opens yaml after it moved to Y")
	}
	press(m, "Y")
	if m.mode != modePager {
		t.Error("Y did not open yaml")
	}
	press(m, "esc")
	if v := m.View(); !strings.Contains(v, "Y yaml") {
		t.Error("header does not show the rebound key")
	}
}

func TestWideShowsExtraColumns(t *testing.T) {
	m := demoModel(t, 220, 40)
	if strings.Contains(m.View(), "SANDBOX") {
		t.Fatal("SANDBOX shown before wide")
	}
	press(m, "w")
	v := m.View()
	if !strings.Contains(v, "SANDBOX") || !strings.Contains(v, "wide") {
		t.Errorf("wide view lacks SANDBOX:\n%s", v)
	}
	// Sorting reaches the wide columns, and leaving wide clamps the sort.
	for range len(m.columns(kActors)) - 1 {
		press(m, "S")
	}
	press(m, "w")
	if m.table().sortCol >= len(lookupKind("actors").columns) {
		t.Errorf("sortCol %d past the normal columns", m.table().sortCol)
	}
	checkFrame(t, m, 220, 40)
	// On a narrow terminal the extra columns run off the edge, and the
	// frame still fits.
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	press(m, "w")
	checkFrame(t, m, 80, 24)
}

func TestCopy(t *testing.T) {
	m := demoModel(t, 120, 40)
	var got []string
	m.opts.Copy = func(s string) error { got = append(got, s); return nil }
	press(m, "c")
	if len(got) != 1 || got[0] != m.selectedID() {
		t.Fatalf("copied %q, want %q", got, m.selectedID())
	}
	if !strings.HasPrefix(m.status, "Copied ") {
		t.Errorf("status %q", m.status)
	}
	press(m, "y", "c")
	if len(got) != 2 || !strings.Contains(got[1], "metadata:") || strings.Contains(got[1], "\x1b[") {
		t.Errorf("yaml copy %q", got[1:])
	}
	m.opts.Copy = nil
	press(m, "esc", "c")
	if !m.statusErr {
		t.Error("copy without a clipboard did not report it")
	}
}

// ctxModel is a demo model with two contexts, each a fresh demo cluster.
func ctxModel(t *testing.T) *Model {
	t.Helper()
	cs := []KubeContext{{Name: "demo", Current: true}, {Name: "demo-staging"}}
	var connected []string
	m := New(Options{
		Connect: func(_ context.Context, name string) (Backend, ConnInfo, error) {
			if name == "" {
				name = "demo"
			}
			connected = append(connected, name)
			return demo.New(), ConnInfo{Context: name}, nil
		},
		Contexts: func() ([]KubeContext, error) { return cs, nil },
		Refresh:  time.Hour,
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	drive(m, m.connect())
	return m
}

func TestSwitchContext(t *testing.T) {
	m := ctxModel(t)
	press(m, ":", "workers", "enter", "/", "gvisor", "enter")
	press(m, ":", "ctx", "enter")
	if m.pager == nil || !m.pager.contexts || !strings.Contains(m.View(), "demo-staging") {
		t.Fatal(":ctx did not list the contexts")
	}
	press(m, "j", "enter")
	if m.info.Context != "demo-staging" || m.backend == nil || m.mode != modeTable {
		t.Fatalf("context %q, backend %v, mode %v", m.info.Context, m.backend, m.mode)
	}
	if m.cur.kind != kWorkers || m.table().filter != "" || len(m.history) != 1 {
		t.Errorf("after switch: %s filter %q history %d", m.cur.kind, m.table().filter, len(m.history))
	}
	if !strings.Contains(m.View(), "demo-staging") {
		t.Error("header does not show the new context")
	}

	press(m, ":", "ctx demo", "enter")
	if m.info.Context != "demo" {
		t.Errorf("`:ctx demo` left us on %q", m.info.Context)
	}
	press(m, ":", "ctx nope", "enter")
	if m.info.Context != "demo" || !m.statusErr {
		t.Errorf("unknown context: on %q, status %q", m.info.Context, m.status)
	}
}

func TestSwitchContextCompletes(t *testing.T) {
	m := ctxModel(t)
	press(m, ":", "ctx st")
	if len(m.suggestions) == 0 || m.suggestions[0] != "ctx demo-staging" {
		t.Errorf("suggestions %v", m.suggestions)
	}
}

func TestSwitchContextWaitsForWrites(t *testing.T) {
	m := ctxModel(t)
	m.batches[99] = &actionBatch{}
	press(m, ":", "ctx demo-staging", "enter")
	if m.info.Context != "demo" || !m.statusErr {
		t.Errorf("switched with a write in flight: %q %q", m.info.Context, m.status)
	}
}

func TestStaleConnectIsDropped(t *testing.T) {
	m := ctxModel(t)
	stale := connectedMsg{epoch: m.epoch - 1, backend: demo.New(), info: ConnInfo{Context: "old"}}
	m.Update(stale)
	if m.info.Context == "old" {
		t.Error("a connect from an earlier epoch replaced the backend")
	}
}

func TestSwitchContextStopsForwards(t *testing.T) {
	m := ctxModel(t)
	m.opts.Write = true
	startForward(t, m, "Running", "0:80")
	f := m.forwards[0]
	press(m, ":", "ctx demo-staging", "enter")
	<-f.Done()
	if len(m.forwards) != 0 {
		t.Errorf("forwards after switch: %d", len(m.forwards))
	}
	last := m.session[len(m.session)-1]
	if last.Action != "proxy stop" || last.Context != "demo" {
		t.Errorf("journal %+v", last)
	}
}

func click(m *Model, x, y int) {
	_, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	drive(m, cmd)
}

func TestMouse(t *testing.T) {
	m := demoModel(t, 120, 40)
	// The third data row sits under the header, the border and the column header.
	click(m, 10, headerHeight+2+2)
	if m.table().cursor != 2 {
		t.Fatalf("click put the cursor on %d", m.table().cursor)
	}
	click(m, 10, headerHeight+2+2)
	if m.mode != modePager {
		t.Fatal("double click did not open the row")
	}
	press(m, "esc")
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.table().cursor != 2+wheelStep {
		t.Errorf("wheel moved the cursor to %d", m.table().cursor)
	}
	click(m, 10, 1) // the header
	if m.table().cursor != 2+wheelStep {
		t.Error("a header click moved the cursor")
	}

	press(m, "u")
	if m.pager == nil || m.pager.items == nil {
		t.Fatal("no relations")
	}
	click(m, 10, headerHeight+1+2) // the second relation
	if m.pager.cursor != 1 {
		t.Errorf("relation cursor %d", m.pager.cursor)
	}
}

func TestApplySkin(t *testing.T) {
	defer ApplySkin(skin.Builtin[skin.Default])
	ApplySkin(skin.Builtin["catppuccin-latte"])
	if string(cRed) != skin.Builtin["catppuccin-latte"].Red {
		t.Errorf("cRed = %s", cRed)
	}
	if got := rowStyles[lvError].GetForeground(); got != cRed {
		t.Errorf("row style not rebuilt: %v", got)
	}
}

func TestHelpListsReboundKeys(t *testing.T) {
	m := demoModel(t, 120, 40)
	km, _ := ParseKeymap(map[string]string{"suspend": "ctrl+s"})
	m.keys = km
	press(m, "?")
	if !strings.Contains(pagerText(m.pager.lines), "^s") {
		t.Error("help does not list the rebound key")
	}
}
