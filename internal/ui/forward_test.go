package ui

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// startForward opens the f dialog on a row in state, sets the ports and
// starts it.
func startForward(t *testing.T, m *Model, state, ports string) string {
	t.Helper()
	press(m, ":", "actors", "enter")
	id := selectState(t, m, 2, state)
	press(m, "f")
	if m.mode != modeForward || m.fwd == nil || m.fwd.id != id {
		t.Fatalf("f: mode %v, dialog %+v", m.mode, m.fwd)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	press(m, ports, "enter")
	return id
}

func httpGet(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text())
	}
	return b.String()
}

func TestReadOnlyBlocksProxy(t *testing.T) {
	m := demoModel(t, 120, 40)
	press(m, ":", "actors", "enter", "f")
	if m.mode != modeTable || m.fwd != nil {
		t.Fatalf("mode %v, want the table", m.mode)
	}
	if !m.statusErr || !strings.Contains(m.status, "read-only") {
		t.Errorf("status %q", m.status)
	}
}

func TestProxyForwardsAndStops(t *testing.T) {
	m := writeModel(t)
	defer m.StopForwards()
	id := startForward(t, m, "Running", "0:80")
	if len(m.forwards) != 1 || m.statusErr {
		t.Fatalf("forwards %d, status %q", len(m.forwards), m.status)
	}
	f := m.forwards[0]
	if f.Actor != id || f.Remote != 80 {
		t.Errorf("forward %s:%d, want %s:80", f.Actor, f.Remote, id)
	}
	if body := httpGet(t, f.Local); !strings.Contains(body, "hello from "+id+" on port 80") {
		t.Errorf("body %q", body)
	}
	if !strings.Contains(m.View(), "⇄ 1") {
		t.Error("header has no forward badge")
	}

	press(m, ":", "pf", "enter")
	if m.pager == nil || !m.pager.forwards {
		t.Fatal(":pf did not open the forward list")
	}
	if v := m.View(); !strings.Contains(v, f.Local) || !strings.Contains(v, id) {
		t.Errorf(":pf does not list %s %s", f.Local, id)
	}
	press(m, "ctrl+d")
	if len(m.forwards) != 0 {
		t.Fatalf("forwards after ctrl-d: %d", len(m.forwards))
	}
	<-f.Done()
	if _, err := net.Dial("tcp", f.Local); err == nil {
		t.Error("still listening after stop")
	}
	var acts []string
	for _, e := range m.session {
		acts = append(acts, e.Action+" "+e.Object+" "+e.Result)
	}
	if want := []string{"proxy " + id + " ok", "proxy stop " + id + " ok"}; strings.Join(acts, "|") != strings.Join(want, "|") {
		t.Errorf("journal %v, want %v", acts, want)
	}
}

func TestProxyEnterJumpsToActor(t *testing.T) {
	m := writeModel(t)
	defer m.StopForwards()
	id := startForward(t, m, "Running", "0:8080")
	press(m, ":", "workers", "enter", ":", "pf", "enter", "enter")
	if m.cur.kind != kActors || m.selectedID() != id {
		t.Errorf("at %s %q, want actors %q", m.cur.kind, m.selectedID(), id)
	}
}

func TestProxyRejectsBadPorts(t *testing.T) {
	m := writeModel(t)
	press(m, ":", "actors", "enter", "f")
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	press(m, "80 80", "enter")
	if m.mode != modeForward || m.fwd == nil || !strings.Contains(m.fwd.err, "duplicate local port 80") {
		t.Fatalf("mode %v, dialog %+v", m.mode, m.fwd)
	}
	press(m, "esc")
	if m.mode != modeTable || len(m.forwards) != 0 {
		t.Errorf("mode %v, forwards %d", m.mode, len(m.forwards))
	}
}

func TestProxyResumesSuspendedActor(t *testing.T) {
	m := writeModel(t)
	defer m.StopForwards()
	press(m, ":", "actors", "enter")
	selectState(t, m, 2, "Suspended")
	press(m, "f")
	if v := m.View(); !strings.Contains(v, "the first connection resumes it") {
		t.Error("dialog does not warn that the proxy resumes the actor")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	press(m, "0:80", "enter")
	id := m.forwards[0].Actor
	httpGet(t, m.forwards[0].Local)
	press(m, "r")
	if got := cell(m, id, 2); got != "Resuming" {
		t.Errorf("state after connect %q, want Resuming", got)
	}
}

func TestProxyDialFailureShowsInList(t *testing.T) {
	m := writeModel(t)
	defer m.StopForwards()
	startForward(t, m, "Crashed", "0:80")
	c, err := net.Dial("tcp", m.forwards[0].Local)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Read(make([]byte, 1)) // closed once the dial fails
	c.Close()
	press(m, ":", "pf", "enter")
	if v := m.View(); !strings.Contains(v, "dial failed") || !strings.Contains(v, "503") {
		t.Errorf(":pf does not show the refused dial:\n%s", v)
	}
}
