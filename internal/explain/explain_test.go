package explain

import (
	"errors"
	"strings"
	"testing"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
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

func TestSandboxConfig(t *testing.T) {
	disabled := infrav1.SandboxVersionStateDisabled
	cfg := func(mut ...func(*infrav1.SandboxConfig)) *infrav1.SandboxConfig {
		c := &infrav1.SandboxConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "gv", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
				Finalizers: []string{infrav1.SandboxConfigWorkerPoolProtectionFinalizer}},
			Spec: infrav1.SandboxConfigSpec{SandboxClass: infrav1.SandboxClassGvisor, DefaultVersion: "v1",
				Versions: []infrav1.SandboxVersionConfig{{Name: "v1"}}},
		}
		for _, m := range mut {
			m(c)
		}
		return c
	}
	pool := func(name string, spec, status string) *infrav1.WorkerPool {
		p := &infrav1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Namespace: "w", Name: name}}
		sc := infrav1.WorkerPoolSandboxClass{Name: infrav1.SandboxClassGvisor}
		if spec != "" {
			sc.ConfigRef = &infrav1.SandboxConfigReference{Name: spec}
		}
		p.Spec.SandboxClasses = []infrav1.WorkerPoolSandboxClass{sc}
		st := infrav1.WorkerPoolSandboxClassStatus{Name: infrav1.SandboxClassGvisor}
		if status != "" {
			st.ConfigRef = &infrav1.SandboxConfigReference{Name: status}
		}
		p.Status.SandboxClasses = []infrav1.WorkerPoolSandboxClassStatus{st}
		return p
	}
	deleting := func(c *infrav1.SandboxConfig) {
		t := metav1.NewTime(now.Add(-10 * time.Minute))
		c.DeletionTimestamp = &t
	}
	tests := []struct {
		name    string
		c       *infrav1.SandboxConfig
		pools   []*infrav1.WorkerPool
		summary string
		finding string
		level   Level
	}{
		{"in use", cfg(), []*infrav1.WorkerPool{pool("a", "", "gv"), pool("b", "other", "other")}, "in use by 1 pool", "1 pool(s) use this config", Info},
		{"unused", cfg(), nil, "not in use", "could not be listed", Info},
		{"named, unresolved", cfg(), []*infrav1.WorkerPool{pool("a", "gv", "old")}, "not in use", "have not resolved it", Warn},
		{"disabled default", cfg(func(c *infrav1.SandboxConfig) { c.Spec.Versions[0].State = &disabled }), []*infrav1.WorkerPool{},
			"no usable default", "names no enabled version", Error},
		{"deleting, held", cfg(deleting), []*infrav1.WorkerPool{pool("a", "", "gv")}, "pools still use it", "1 pool(s) hold it", Warn},
		{"deleting, finalizer", cfg(deleting), []*infrav1.WorkerPool{}, "being deleted", "atecontroller", Warn},
	}
	for _, tt := range tests {
		r := SandboxConfig(tt.c, tt.pools, now)
		var titles []string
		for _, f := range r.Findings {
			titles = append(titles, f.Title)
		}
		all := strings.Join(titles, "\n")
		if !strings.Contains(r.Summary, tt.summary) || !strings.Contains(all, tt.finding) || r.Level != tt.level {
			t.Errorf("%s: summary %q level %v findings:\n%s", tt.name, r.Summary, r.Level, all)
		}
	}
}

func TestTransitionWithoutUpdateTime(t *testing.T) {
	for _, s := range []ateapi.ActorState{ateapi.ActorState_ACTOR_STATE_RESUMING, ateapi.ActorState_ACTOR_STATE_SUSPENDING,
		ateapi.ActorState_ACTOR_STATE_REVERTING, ateapi.ActorState_ACTOR_STATE_DELETING} {
		a := actor(s, 0, func(a *ateapi.Actor) { a.Metadata.UpdateTime, a.Metadata.CreateTime = nil, nil })
		r := Actor(ActorInput{Actor: a, Now: now})
		for _, f := range r.Findings {
			if strings.Contains(f.Title, " for ") {
				t.Errorf("%v: finding %q claims a duration without update_time", s, f.Title)
			}
		}
		if r.Level == Warn || r.Level == Error {
			t.Errorf("%v: level %v, want no warning: %+v", s, r.Level, r.Findings)
		}
		for _, f := range r.Facts {
			if (f.Label == "Updated" || f.Label == "Created") && f.Value != "-" {
				t.Errorf("%v: fact %s = %q, want -", s, f.Label, f.Value)
			}
		}
	}
}

func TestSnapshotsNotMutated(t *testing.T) {
	a := actor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, time.Minute, func(a *ateapi.Actor) {
		a.Status.Snapshots = []*ateapi.Snapshot{{Generation: 1}, {Generation: 3}, {Generation: 2}}
	})
	r := Actor(ActorInput{Actor: a, Now: now})
	for i, want := range []int32{1, 3, 2} {
		if g := a.Status.Snapshots[i].GetGeneration(); g != want {
			t.Fatalf("snapshot %d is gen %d, want %d: Actor reordered the caller's slice", i, g, want)
		}
	}
	for _, f := range r.Findings {
		if strings.Contains(f.Title, "snapshot generation") && !strings.HasPrefix(f.Detail[0], "gen 3") {
			t.Errorf("detail not newest first: %q", f.Detail)
		}
	}
}

func TestSeriesEvent(t *testing.T) {
	// An events.k8s.io recorder: EventTime is the first occurrence,
	// repeats go in Series, LastTimestamp stays zero.
	e := corev1.Event{Type: corev1.EventTypeWarning, Reason: "FailedScheduling", Message: "0/3 nodes",
		EventTime: metav1.NewMicroTime(now.Add(-3 * time.Hour)),
		Series:    &corev1.EventSeries{Count: 40, LastObservedTime: metav1.NewMicroTime(now.Add(-2 * time.Minute))}}
	if got := EventTime(&e); !got.Equal(now.Add(-2 * time.Minute)) {
		t.Errorf("EventTime = %v, want Series.LastObservedTime", got)
	}
	if got := EventCount(&e); got != 40 {
		t.Errorf("EventCount = %d, want 40", got)
	}
	if got := EventCount(&corev1.Event{Count: 5, Series: &corev1.EventSeries{Count: 2}}); got != 5 {
		t.Errorf("EventCount = %d, want the larger Count 5", got)
	}
	running := actor(ateapi.ActorState_ACTOR_STATE_RUNNING, time.Minute, func(a *ateapi.Actor) {
		a.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: &ateapi.ObjectRef{Name: "w1"}, WorkerNamespace: "ns", WorkerPod: "w1-pod"}
	})
	r := Actor(ActorInput{Actor: running, PodEvents: []corev1.Event{e}, Now: now})
	found := false
	for _, f := range r.Findings {
		if strings.Contains(f.Title, "Warning event") {
			found = true
			if len(f.Detail) != 1 || f.Detail[0] != "FailedScheduling x40, 2m ago: 0/3 nodes" {
				t.Errorf("detail %q", f.Detail)
			}
		}
	}
	if !found {
		t.Errorf("recurring series warning was filtered out: %+v", r.Findings)
	}
}

func TestBytesRounding(t *testing.T) {
	for in, want := range map[int64]string{1048575: "1Mi", 1023: "1023B", 1024: "1Ki", 1<<30 - 1: "1Gi", 1<<20 + 1<<19: "1.5Mi", 1 << 62: "4Ei"} {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSandboxConfigDeletingPoolsUnknown(t *testing.T) {
	d := metav1.NewTime(now.Add(-10 * time.Minute))
	c := &infrav1.SandboxConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "gv", DeletionTimestamp: &d, Finalizers: []string{infrav1.SandboxConfigWorkerPoolProtectionFinalizer}},
		Spec:       infrav1.SandboxConfigSpec{SandboxClass: infrav1.SandboxClassGvisor, DefaultVersion: "v1", Versions: []infrav1.SandboxVersionConfig{{Name: "v1"}}},
	}
	r := SandboxConfig(c, nil, now)
	for _, f := range r.Findings {
		if strings.Contains(f.Title, "atecontroller") || strings.Contains(strings.Join(f.Detail, " "), "atecontroller is running") {
			t.Errorf("blames atecontroller when pools are unknown: %q %q", f.Title, f.Detail)
		}
	}
	if !hasFinding(r, Warn, "Deleting since 10m ago") {
		t.Errorf("want deleting warning: %+v", r.Findings)
	}
}
