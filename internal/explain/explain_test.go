package explain

import (
	"errors"
	"strings"
	"testing"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func actor(state ateapi.ActorState, updatedAgo time.Duration, mut ...func(*ateapi.Actor)) *ateapi.Actor {
	a := &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{
			Atespace: "demo", Name: "a1",
			CreateTime: timestamppb.New(now.Add(-time.Hour)),
			UpdateTime: timestamppb.New(now.Add(-updatedAgo)),
		},
		ActorTemplate: &ateapi.ObjectRef{Atespace: "demo", Name: "tpl"},
		Status:        &ateapi.ActorStatus{State: state},
	}
	for _, m := range mut {
		m(a)
	}
	return a
}

func worker(name string, state ateapi.WorkerState, used, cap int32, labels map[string]string) *ateapi.Worker {
	return &ateapi.Worker{
		Metadata:  &ateapi.ResourceMetadata{Name: name},
		WorkerPod: name + "-pod",
		Labels:    labels,
		Status: &ateapi.WorkerStatus{
			State:     state,
			Capacity:  &ateapi.WorkerResources{Actors: cap},
			Allocated: &ateapi.WorkerResources{Actors: used},
		},
	}
}

func hasFinding(r Report, l Level, substr string) bool {
	for _, f := range r.Findings {
		if f.Level == l && strings.Contains(f.Title, substr) {
			return true
		}
	}
	return false
}

func TestActor(t *testing.T) {
	active := ateapi.WorkerState_WORKER_STATE_ACTIVE
	tests := []struct {
		name    string
		in      ActorInput
		level   Level
		finding string
	}{
		{
			name: "crashed reports message",
			in: ActorInput{Actor: actor(ateapi.ActorState_ACTOR_STATE_CRASHED, time.Minute, func(a *ateapi.Actor) {
				a.Status.Crash = &ateapi.ActorCrash{Message: "exit code 137", CrashTime: timestamppb.New(now.Add(-time.Minute))}
			})},
			level: Error, finding: "exit code 137",
		},
		{
			name:  "resuming briefly is info",
			in:    ActorInput{Actor: actor(ateapi.ActorState_ACTOR_STATE_RESUMING, 5*time.Second)},
			level: Info, finding: "Resuming for 5s",
		},
		{
			name:  "resuming too long is stuck",
			in:    ActorInput{Actor: actor(ateapi.ActorState_ACTOR_STATE_RESUMING, 10*time.Minute)},
			level: Warn, finding: "Resuming for 10m",
		},
		{
			name: "suspended with no matching worker",
			in: ActorInput{
				Actor: actor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, time.Hour, func(a *ateapi.Actor) {
					a.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"gpu": "true"}}
				}),
				Workers: []*ateapi.Worker{worker("w1", active, 0, 4, nil)},
			},
			level: Error, finding: "No worker matches selector gpu=true",
		},
		{
			name: "template selector applies when actor has none",
			in: ActorInput{
				Actor:    actor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, time.Hour),
				Template: &ateapi.ActorTemplate{WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"pool": "big"}}},
				Workers:  []*ateapi.Worker{worker("w1", active, 0, 4, map[string]string{"pool": "small"})},
			},
			level: Error, finding: "No worker matches selector pool=big",
		},
		{
			name: "suspended with full workers",
			in: ActorInput{
				Actor:   actor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, time.Hour),
				Workers: []*ateapi.Worker{worker("w1", active, 4, 4, nil)},
			},
			level: Warn, finding: "are full",
		},
		{
			name: "running on missing worker",
			in: ActorInput{
				Actor: actor(ateapi.ActorState_ACTOR_STATE_RUNNING, time.Minute, func(a *ateapi.Actor) {
					a.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: &ateapi.ObjectRef{Name: "gone"}, WorkerPod: "gone-pod"}
				}),
				Workers: []*ateapi.Worker{worker("w1", active, 1, 4, nil)},
			},
			level: Error, finding: "not registered",
		},
		{
			name: "running on draining worker",
			in: ActorInput{
				Actor: actor(ateapi.ActorState_ACTOR_STATE_RUNNING, time.Minute, func(a *ateapi.Actor) {
					a.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: &ateapi.ObjectRef{Name: "w1"}}
				}),
				Workers: []*ateapi.Worker{worker("w1", ateapi.WorkerState_WORKER_STATE_DRAINING, 1, 4, nil)},
			},
			level: Warn, finding: "draining",
		},
		{
			name: "template fetch error",
			in: ActorInput{
				Actor:       actor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, time.Hour),
				TemplateErr: errors.New("not found"),
			},
			level: Error, finding: "could not be loaded",
		},
		{
			name: "no durable snapshot",
			in: ActorInput{Actor: actor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, time.Hour, func(a *ateapi.Actor) {
				a.Status.AssignedNode = "node-1"
				a.Status.Snapshots = []*ateapi.Snapshot{{Generation: 1}}
			})},
			level: Warn, finding: "No durable snapshot",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.Now = now
			r := Actor(tt.in)
			if r.Level != tt.level {
				t.Errorf("Level = %v, want %v; findings %+v", r.Level, tt.level, r.Findings)
			}
			if !hasFinding(r, tt.level, tt.finding) {
				t.Errorf("no %v finding containing %q in %+v", tt.level, tt.finding, r.Findings)
			}
		})
	}
}

func TestPodWarningEvents(t *testing.T) {
	ev := func(typ, reason string, ago time.Duration) corev1.Event {
		return corev1.Event{Type: typ, Reason: reason, Message: reason + " message", Count: 2, LastTimestamp: metav1.NewTime(now.Add(-ago))}
	}
	running := actor(ateapi.ActorState_ACTOR_STATE_RUNNING, time.Minute, func(a *ateapi.Actor) {
		a.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: &ateapi.ObjectRef{Name: "w1"}, WorkerNamespace: "ns", WorkerPod: "w1-pod"}
	})
	tests := []struct {
		name   string
		events []corev1.Event
		warn   bool
	}{
		{"none fetched", nil, false},
		{"normal only", []corev1.Event{ev(corev1.EventTypeNormal, "Started", time.Minute)}, false},
		{"old warning", []corev1.Event{ev(corev1.EventTypeWarning, "BackOff", 2*time.Hour)}, false},
		{"recent warning", []corev1.Event{ev(corev1.EventTypeNormal, "Started", time.Minute), ev(corev1.EventTypeWarning, "Unhealthy", 4*time.Minute)}, true},
	}
	for _, tt := range tests {
		r := Actor(ActorInput{Actor: running, PodEvents: tt.events, Now: now})
		if got := hasFinding(r, Warn, "Warning event(s) on worker pod ns/w1-pod"); got != tt.warn {
			t.Errorf("actor, %s: warning finding %v, want %v: %+v", tt.name, got, tt.warn, r.Findings)
		}
		w := worker("w1", ateapi.WorkerState_WORKER_STATE_ACTIVE, 1, 4, nil)
		w.WorkerNamespace = "ns"
		r = Worker(WorkerInput{Worker: w, PodEvents: tt.events, Now: now})
		if got := hasFinding(r, Warn, "Warning event(s)"); got != tt.warn {
			t.Errorf("worker, %s: warning finding %v, want %v", tt.name, got, tt.warn)
		}
	}
	r := Actor(ActorInput{Actor: running, PodEvents: tests[3].events, Now: now})
	for _, f := range r.Findings {
		if strings.Contains(f.Title, "Warning event") && (len(f.Detail) != 1 || f.Detail[0] != "Unhealthy x2, 4m ago: Unhealthy message") {
			t.Errorf("detail %q", f.Detail)
		}
	}
}

func TestWorkerFull(t *testing.T) {
	r := Worker(WorkerInput{Worker: worker("w1", ateapi.WorkerState_WORKER_STATE_ACTIVE, 4, 4, nil), Now: now})
	if !hasFinding(r, Warn, "Full") {
		t.Errorf("want Full warning, got %+v", r.Findings)
	}
}

func TestBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "-", 512: "512B", 1 << 30: "1Gi", 1536 << 20: "1.5Gi"} {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}
