package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ate-tooling/ate-tui/internal/demo"
	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// historyBackend has one worker, w1 on pod-a, and the given live actors.
type historyBackend struct {
	*logBackend
	actors []*ateapi.Actor
}

func (b *historyBackend) ListWorkers(context.Context) ([]*ateapi.Worker, error) {
	return []*ateapi.Worker{{Metadata: &ateapi.ResourceMetadata{Name: "w1"}, WorkerNamespace: "ate-workers", WorkerPod: "pod-a"}}, nil
}

func (b *historyBackend) ListActors(context.Context, string) ([]*ateapi.Actor, error) {
	return b.actors, nil
}

// actorLine is a kubelet-timestamped worker line for any actor.
func actorLine(at time.Duration, atespace, name, msg string) string {
	return t0.Add(at).Format(time.RFC3339Nano) + " " +
		fmt.Sprintf(`{"message":%q,"labels":{"ate.atespace":%q,"ate.actor.name":%q,"ate.actor.container.name":"agent"}}`, msg, atespace, name)
}

func historyModel(t *testing.T) (*Model, *historyBackend) {
	t.Helper()
	live := logActor(ateapi.ActorState_ACTOR_STATE_RUNNING, "pod-a")
	live.Metadata.CreateTime = timestamppb.New(t0.Add(time.Minute))
	b := &historyBackend{logBackend: &logBackend{Backend: demo.New(), lines: map[string][]string{"pod-a": {
		// An earlier research/a1, deleted before the live one was created.
		actorLine(0, "research", "a1", "old one"),
		actorLine(time.Second, "support", "gone", "g1"),
		actorLine(2*time.Second, "support", "gone", "g2"),
		actorLine(3*time.Second, "research", "a1", "old two"),
		`{"message":"sandbox","labels":{"ate.atespace":"research","ate.actor.name":"a1","ate.actor.container.name":"_sandbox"}}`,
		"not json",
		actorLine(2*time.Minute, "research", "a1", "new one"),
	}}}, actors: []*ateapi.Actor{live}}
	m := New(Options{
		Connect: func(context.Context, string) (Backend, ConnInfo, error) { return b, ConnInfo{Context: "test"}, nil },
		Refresh: time.Hour,
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(m.connect()())
	m.worker = "w1"
	m.Update(run(t, m.gotoKind(kHistory, "")))
	return m, b
}

func TestHistoryRows(t *testing.T) {
	m, _ := historyModel(t)
	var got []string
	for _, r := range m.data[m.cur].rows {
		got = append(got, strings.Join(r.cells[:4], " "))
	}
	want := []string{"research a1 Running 1", "research a1 Deleted 2", "support gone Deleted 2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows %q, want %q", got, want)
	}
	if !strings.Contains(m.View(), "worker w1") {
		t.Error("header does not name the worker")
	}
}

func TestHistoryKeptLogs(t *testing.T) {
	m, b := historyModel(t)
	rows := m.data[m.cur].rows
	cmd := m.openKept(rows[1].obj.(*keptActor))
	if !m.logs.kept || m.pollLogActor() != nil {
		t.Fatal("kept logs poll the actor")
	}
	for cmd != nil {
		_, cmd = m.Update(run(t, cmd))
	}
	want := []string{"actor deleted; these lines are what ate-workers/pod-a kept", "old one", "old two"}
	if got := msgs(m); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries %q, want %q", got, want)
	}
	if c := b.call(len(b.calls) - 1); c.opts.Follow {
		t.Fatal("kept logs follow the pod")
	}
	if !strings.Contains(m.View(), "kept by ate-workers/pod-a") {
		t.Error("title does not say where the lines are kept")
	}

	// The live actor on this pod is followed like l on the actor.
	m.closePager()
	m.openKept(rows[0].obj.(*keptActor))
	if m.logs.kept {
		t.Fatal("the running actor was not followed")
	}
	m.closePager()
}

func TestWorkerLogsKey(t *testing.T) {
	m, _ := historyModel(t)
	m.worker = ""
	// connect's fetches were never run.
	m.ds(viewKey{kWorkers, ""}).loading = false
	m.Update(run(t, m.gotoKind(kWorkers, "")))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if m.cur != (viewKey{kHistory, "w1"}) || cmd == nil {
		t.Fatalf("l on a worker went to %v", m.cur)
	}
}
