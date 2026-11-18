package ui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// writeModel is a demo model with writes on and an immediate re-poll.
func writeModel(t *testing.T) *Model {
	t.Helper()
	m := demoModel(t, 120, 40)
	m.opts.Write = true
	m.repoll = 0
	return m
}

// cell is a column of the row id in the current view, or "" when it is gone.
func cell(m *Model, id string, col int) string {
	for _, r := range m.data[m.cur].rows {
		if r.id == id {
			return r.cells[col]
		}
	}
	return ""
}

// selectState puts the cursor on a row whose state column is state.
func selectState(t *testing.T, m *Model, col int, state string) string {
	t.Helper()
	tb := m.table()
	d := m.data[m.cur]
	for i, vi := range tb.visible {
		if d.rows[vi].cells[col] == state {
			tb.cursor = i
			return d.rows[vi].id
		}
	}
	t.Fatalf("no %s row in %s", state, m.cur.kind)
	return ""
}

func TestReadOnlyBlocksActions(t *testing.T) {
	tests := []struct {
		view string
		key  string
	}{
		{"actors", "s"}, {"actors", "R"}, {"actors", "v"}, {"actors", "ctrl+d"},
		{"workers", "D"}, {"workers", "A"}, {"workers", "U"},
	}
	m := demoModel(t, 120, 40)
	for _, tt := range tests {
		press(m, ":", tt.view, "enter", tt.key)
		if m.mode != modeTable || m.confirm != nil {
			t.Errorf("%s %s: mode %v, want the table", tt.view, tt.key, m.mode)
		}
		if !strings.Contains(m.status, "--write") || !m.statusErr {
			t.Errorf("%s %s: status %q", tt.view, tt.key, m.status)
		}
	}
	if !strings.Contains(m.View(), "[read-only]") {
		t.Error("header has no [read-only] badge")
	}
}

func TestWriteContextsAllowlist(t *testing.T) {
	m := demoModel(t, 120, 40)
	m.opts.WriteContexts = []string{"prod"}
	if m.writable() {
		t.Fatal("writable on a context outside the allowlist")
	}
	m.opts.WriteContexts = []string{"demo"}
	if !m.writable() || !strings.Contains(m.View(), "[write]") {
		t.Fatal("not writable on an allowlisted context")
	}
}

func TestConfirmThenCancelDoesNothing(t *testing.T) {
	for _, cancel := range []string{"n", "esc", "q"} {
		m := writeModel(t)
		id := selectState(t, m, 2, "Running")
		press(m, "s")
		if m.mode != modeConfirm {
			t.Fatalf("%s: dialog did not open", cancel)
		}
		press(m, "j", "G", cancel)
		if m.mode != modeTable || len(m.session) != 0 || cell(m, id, 2) != "Running" {
			t.Errorf("%s: mode %v, journal %d, state %s", cancel, m.mode, len(m.session), cell(m, id, 2))
		}
	}
}

func TestSuspendChangesState(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		now  string // state right after the RPC
		mode string
	}{
		{"async", []string{"s", "y"}, "Suspending", "async"},
		{"sync", []string{"s", "tab", "enter"}, "Suspended", "sync"},
	}
	for _, tt := range tests {
		m := writeModel(t)
		id := selectState(t, m, 2, "Running")
		press(m, tt.keys...)
		if got := cell(m, id, 2); got != tt.now {
			t.Errorf("%s: state %s, want %s (status %q)", tt.name, got, tt.now, m.status)
		}
		drive(m, m.fetch(m.cur))
		if got := cell(m, id, 2); got != "Suspended" {
			t.Errorf("%s: after polls, state %s, want Suspended", tt.name, got)
		}
		if m.status != "suspended "+id || len(m.session) != 1 || m.session[0].Mode != tt.mode {
			t.Errorf("%s: status %q, journal %+v", tt.name, m.status, m.session)
		}
	}
}

func TestResumeCrashedFails(t *testing.T) {
	m := writeModel(t)
	id := selectState(t, m, 2, "Crashed")
	press(m, "R", "y")
	if !m.statusErr || !strings.Contains(m.status, "revert it") || cell(m, id, 2) != "Crashed" {
		t.Fatalf("status %q, state %s", m.status, cell(m, id, 2))
	}
	press(m, "v", "y")
	if got := cell(m, id, 2); got != "Suspended" || m.statusErr {
		t.Fatalf("after revert: state %s, status %q", got, m.status)
	}
}

func TestDeleteNeedsTypedName(t *testing.T) {
	m := writeModel(t)
	id := selectState(t, m, 2, "Suspended")
	name := id[strings.Index(id, "/")+1:]

	press(m, "ctrl+d", "wrong", "enter")
	if m.mode != modeConfirm || !m.confirm.refused || cell(m, id, 2) != "Suspended" {
		t.Fatalf("wrong name: mode %v, state %s", m.mode, cell(m, id, 2))
	}
	// Every other key is text while the dialog is open.
	press(m, "q", "y", ":", "tab")
	if m.mode != modeConfirm || m.input != "wrongqy:" {
		t.Fatalf("mode %v, input %q", m.mode, m.input)
	}
	press(m, "esc", "ctrl+d", name, "enter")
	if m.mode != modeTable || cell(m, id, 2) != "Deleting" {
		t.Fatalf("mode %v, state %s", m.mode, cell(m, id, 2))
	}
	drive(m, m.fetch(m.cur))
	if got := cell(m, id, 2); got != "" {
		t.Fatalf("deleted actor still listed as %s", got)
	}
}

func TestDeleteJournalsWorkerPod(t *testing.T) {
	m := writeModel(t)
	id := selectState(t, m, 2, "Running")
	a := m.selected().obj.(*ateapi.Actor)
	wa := a.GetStatus().GetWorkerAssignment()
	press(m, "ctrl+d", a.GetMetadata().GetName(), "enter")
	if len(m.session) != 1 {
		t.Fatalf("journal %+v", m.session)
	}
	e := m.session[0]
	if e.Object != id || e.Worker != wa.GetWorker().GetName() || e.Pod != wa.GetWorkerNamespace()+"/"+wa.GetWorkerPod() {
		t.Fatalf("journal entry %+v, want worker %s pod %s", e, wa.GetWorker().GetName(), wa.GetWorkerPod())
	}
	if !strings.Contains(strings.Join(m.journalLines(), "\n"), e.Pod) {
		t.Error(":journal does not show the pod")
	}
}

func TestBulkMarkedApplyToAll(t *testing.T) {
	m := writeModel(t)
	press(m, ":", "workers", "enter")
	// Workers sort by name; the fourth is DRAINING and cannot be marked.
	press(m, "space", "j", "space", "j", "space", "j", "space")
	if n := len(m.table().marked); n != 4 {
		t.Fatalf("marked %d, want 4", n)
	}
	drive(m, m.fetch(m.cur))
	if n := len(m.table().marked); n != 4 {
		t.Fatalf("marks lost on refresh: %d", n)
	}
	press(m, "U")
	if m.mode != modeConfirm || len(m.confirm.targets) != 4 {
		t.Fatalf("dialog: mode %v, %+v", m.mode, m.confirm)
	}
	press(m, "y")
	if !strings.HasPrefix(m.status, "marked unavailable 3, failed 1: ") || !strings.Contains(m.status, "DRAINING") {
		t.Fatalf("status %q", m.status)
	}
	if len(m.table().marked) != 0 {
		t.Fatal("marks not cleared after the action")
	}
	unavailable := 0
	for _, r := range m.data[m.cur].rows {
		if r.cells[1] == "Unavailable" {
			unavailable++
		}
	}
	if unavailable != 3 {
		t.Fatalf("%d workers unavailable, want 3", unavailable)
	}

	press(m, "space", ":", "actors", "enter", ":", "workers", "enter")
	if len(m.table().marked) != 0 {
		t.Fatal("marks survived a view change")
	}
}

func TestActionErrHints(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{status.Error(codes.PermissionDenied, "denied"), "ate-client principal"},
		{status.Error(codes.Unauthenticated, "bad token"), "--token-file"},
		{status.Error(codes.FailedPrecondition, "actor is CRASHED"), "actor is CRASHED"},
		{errors.New("plain"), "plain"},
	}
	for _, tt := range tests {
		if got := actionErr(tt.err); !strings.Contains(got, tt.want) || strings.Contains(got, "rpc error") {
			t.Errorf("%v: got %q", tt.err, got)
		}
	}
}

func TestJournalPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "journal.jsonl")
	m := writeModel(t)
	m.opts.JournalPath = path
	id := selectState(t, m, 2, "Running")
	press(m, "s", "y")
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"object":"`+id+`"`) {
		t.Fatalf("journal %q, %v", b, err)
	}

	// A new session lists the earlier entry.
	m2 := writeModel(t)
	m2.opts.JournalPath = path
	press(m2, ":", "journal", "enter")
	if m2.pager == nil || !strings.Contains(strings.Join(m2.pager.lines, "\n"), id) {
		t.Fatalf("journal pager does not list %s", id)
	}

	// A write failure shows in the status line.
	m.opts.JournalPath = filepath.Join(path, "not-a-dir", "j.jsonl")
	selectState(t, m, 2, "Running")
	press(m, "s", "y")
	if !strings.Contains(m.status, "journal:") {
		t.Fatalf("status %q", m.status)
	}
}
