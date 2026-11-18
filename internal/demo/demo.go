// Package demo is an in-memory Backend with sample data that changes over
// time, for trying the UI without a cluster.
package demo

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Backend struct {
	mu        sync.Mutex
	start     time.Time
	rng       *rand.Rand
	atespaces []*ateapi.Atespace
	templates []*ateapi.ActorTemplate
	actors    []*ateapi.Actor
	workers   []*ateapi.Worker
	pools     []infrav1.WorkerPool
	// pinned actors keep their demo state; step never moves them.
	pinned map[*ateapi.Actor]bool
	// pending counts the polls left before a transition started by a write
	// completes, so the UI sees SUSPENDING before SUSPENDED.
	pending map[*ateapi.Actor]int
}

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

func New() *Backend {
	now := time.Now()
	b := &Backend{start: now, rng: rand.New(rand.NewSource(7)),
		pinned: map[*ateapi.Actor]bool{}, pending: map[*ateapi.Actor]int{}}
	md := func(as, name string, age time.Duration) *ateapi.ResourceMetadata {
		return &ateapi.ResourceMetadata{Atespace: as, Name: name, Uid: fmt.Sprintf("%08x-%04x", b.rng.Uint32(), b.rng.Intn(65535)),
			CreateTime: ts(now.Add(-age)), UpdateTime: ts(now.Add(-age / 3))}
	}
	for _, a := range []string{"research", "support", "codegen"} {
		b.atespaces = append(b.atespaces, &ateapi.Atespace{Metadata: md("", a, 72*time.Hour)})
	}
	tpls := map[string][]string{
		"research": {"browser-agent", "summarizer"},
		"support":  {"triage-bot"},
		"codegen":  {"repo-agent", "test-runner"},
	}
	for as, names := range tpls {
		for _, n := range names {
			t := &ateapi.ActorTemplate{
				Metadata:   md(as, n, 48*time.Hour),
				Containers: []*ateapi.Container{{Name: "agent", Image: "ghcr.io/example/" + n + ":v1.4"}},
				Resources:  &ateapi.ResourceRequirements{Limits: &ateapi.Resources{CpuMillicores: 1000, MemoryBytes: 2 << 30}},
				Status: &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{
					GoldenTag: &ateapi.ObjectRef{Atespace: as, Name: n + "-golden"}, SnapshotTime: ts(now.Add(-40 * time.Hour)),
				}},
			}
			if n == "test-runner" {
				t.Status.GoldenSnapshotStatus = &ateapi.GoldenSnapshotStatus{ErrorMessage: "wakeup probe GET :8080/healthz timed out after 30s"}
			}
			b.templates = append(b.templates, t)
		}
	}
	for i := 0; i < 6; i++ {
		pool := "pool-gvisor"
		labels := map[string]string{"sandbox": "gvisor"}
		if i >= 4 {
			pool = "pool-microvm"
			labels = map[string]string{"sandbox": "microvm"}
		}
		w := &ateapi.Worker{
			Metadata:        md("", fmt.Sprintf("worker-%s-%d", pool[5:], i), 30*time.Hour),
			WorkerNamespace: "ate-workers", WorkerPool: pool,
			WorkerPod: fmt.Sprintf("%s-%05x", pool, b.rng.Intn(1<<20)), NodeName: fmt.Sprintf("gke-node-%d", i/2),
			Labels: labels,
			Status: &ateapi.WorkerStatus{
				State:       ateapi.WorkerState_WORKER_STATE_ACTIVE,
				Capacity:    &ateapi.WorkerResources{Actors: 4, CpuMillicores: 8000, MemoryBytes: 16 << 30},
				IssuedEpoch: 3, RegisteredEpoch: 3, ObservedEpoch: 3,
			},
		}
		b.workers = append(b.workers, w)
	}
	b.workers[3].Status.State = ateapi.WorkerState_WORKER_STATE_DRAINING
	for _, p := range []string{"pool-gvisor", "pool-microvm"} {
		ready := int32(4)
		if p == "pool-microvm" {
			ready = 2
		}
		b.pools = append(b.pools, infrav1.WorkerPool{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ate-workers", Name: p, CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Hour))},
			Spec:       infrav1.WorkerPoolSpec{Replicas: map[bool]int32{true: 4, false: 3}[p == "pool-gvisor"], WorkerImage: "ghcr.io/agent-substrate/ateworker:v0.6.0"},
			Status: infrav1.WorkerPoolStatus{Replicas: ready + map[bool]int32{true: 0, false: 1}[p == "pool-gvisor"], ReadyReplicas: ready,
				SandboxClasses: []infrav1.WorkerPoolSandboxClassStatus{{
					Name:       infrav1.SandboxClass(strings.TrimPrefix(p, "pool-")),
					Conditions: []metav1.Condition{{Type: infrav1.WorkerPoolSandboxClassConditionResolved, Status: metav1.ConditionTrue, Reason: "Resolved"}},
				}}},
		})
	}
	for i := 0; i < 26; i++ {
		t := b.templates[i%len(b.templates)]
		a := &ateapi.Actor{
			Metadata:      md(t.Metadata.Atespace, fmt.Sprintf("%s-%s", t.Metadata.Name, randSuffix(b.rng)), time.Duration(1+i)*time.Hour),
			ActorTemplate: &ateapi.ObjectRef{Atespace: t.Metadata.Atespace, Name: t.Metadata.Name},
			Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED},
		}
		b.actors = append(b.actors, a)
	}
	for i := 0; i < 9; i++ {
		b.place(b.actors[i*2], b.workers[i%3])
	}
	crash := b.actors[5]
	crash.Status = &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_CRASHED, LastAssignedGeneration: 3,
		Crash: &ateapi.ActorCrash{Message: "container agent exited with code 137 (OOMKilled)", CrashTime: ts(now.Add(-12 * time.Minute))}}
	stuck := b.actors[7]
	stuck.Status.State = ateapi.ActorState_ACTOR_STATE_RESUMING
	stuck.Metadata.UpdateTime = ts(now.Add(-6 * time.Minute))
	b.actors[11].WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"gpu": "a100"}}
	b.pinned[crash], b.pinned[stuck], b.pinned[b.actors[11]] = true, true, true
	for _, a := range b.actors {
		if a.Status.State == ateapi.ActorState_ACTOR_STATE_SUSPENDED && b.rng.Intn(2) == 0 {
			a.Status.LastAssignedGeneration = int32(1 + b.rng.Intn(5))
			a.Status.AssignedNode = fmt.Sprintf("gke-node-%d", b.rng.Intn(3))
			a.Status.Snapshots = []*ateapi.Snapshot{{Generation: a.Status.LastAssignedGeneration, Fidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
				Locality: a.Status.AssignedNode, DurableSnapshot: &ateapi.SnapshotStorage{State: ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED}}}
		}
	}
	b.recount()
	return b
}

func randSuffix(r *rand.Rand) string {
	const chars = "bcdfghjklmnpqrstvwxz2456789"
	s := make([]byte, 5)
	for i := range s {
		s[i] = chars[r.Intn(len(chars))]
	}
	return string(s)
}

func (b *Backend) place(a *ateapi.Actor, w *ateapi.Worker) {
	a.Status.State = ateapi.ActorState_ACTOR_STATE_RUNNING
	a.Status.AssignedNode = w.NodeName
	a.Status.WorkerAssignment = &ateapi.WorkerAssignment{
		Worker: &ateapi.ObjectRef{Name: w.Metadata.Name}, WorkerNamespace: w.WorkerNamespace,
		WorkerPool: w.WorkerPool, WorkerPod: w.WorkerPod, NodeName: w.NodeName,
		SandboxRuntime: &ateapi.ActorSandboxRuntime{SandboxClass: w.Labels["sandbox"]},
	}
	a.Metadata.UpdateTime = ts(time.Now())
}

func (b *Backend) recount() {
	for _, w := range b.workers {
		n := int32(0)
		for _, a := range b.actors {
			if a.Status.GetWorkerAssignment().GetWorker().GetName() == w.Metadata.Name {
				n++
			}
		}
		w.Status.Allocated = &ateapi.WorkerResources{Actors: n, CpuMillicores: int64(n) * 1000, MemoryBytes: int64(n) * 2 << 30}
	}
}

// step moves a few actors through their lifecycle so the UI has something
// to show live, and finishes transitions started by writes.
func (b *Backend) step() {
	now := time.Now()
	kept := b.actors[:0]
	for _, a := range b.actors {
		if n, ok := b.pending[a]; ok {
			if n > 1 {
				b.pending[a] = n - 1
				kept = append(kept, a)
				continue
			}
			delete(b.pending, a)
			if a.Status.State == ateapi.ActorState_ACTOR_STATE_DELETING {
				continue // gone
			}
			b.finish(a, now)
			kept = append(kept, a)
			continue
		}
		kept = append(kept, a)
		if b.pinned[a] || b.rng.Intn(12) != 0 {
			continue
		}
		switch a.Status.State {
		case ateapi.ActorState_ACTOR_STATE_RUNNING:
			a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDING
		case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
			a.Status.State = ateapi.ActorState_ACTOR_STATE_RESUMING
		default:
			b.finish(a, now)
			continue
		}
		a.Metadata.UpdateTime = ts(now)
	}
	b.actors = kept
	b.recount()
}

// finish completes an in-progress transition.
func (b *Backend) finish(a *ateapi.Actor, now time.Time) {
	switch a.Status.State {
	case ateapi.ActorState_ACTOR_STATE_SUSPENDING, ateapi.ActorState_ACTOR_STATE_REVERTING:
		b.suspended(a)
	case ateapi.ActorState_ACTOR_STATE_RESUMING:
		if w := b.pickWorker(a); w != nil {
			b.place(a, w)
		}
		return // no free worker: stays RESUMING, as a real stuck resume would
	default:
		return
	}
	a.Metadata.UpdateTime = ts(now)
}

func (b *Backend) suspended(a *ateapi.Actor) {
	a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
	a.Status.WorkerAssignment = nil
	a.Status.LastAssignedGeneration++
}

// pickWorker finds an ACTIVE worker with a free slot that matches the
// actor's selector.
func (b *Backend) pickWorker(a *ateapi.Actor) *ateapi.Worker {
	for _, i := range b.rng.Perm(len(b.workers)) {
		w := b.workers[i]
		if w.Status.State != ateapi.WorkerState_WORKER_STATE_ACTIVE || w.Status.GetAllocated().GetActors() >= w.Status.GetCapacity().GetActors() {
			continue
		}
		ok := true
		for k, v := range a.GetWorkerSelector().GetMatchLabels() {
			if w.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			return w
		}
	}
	return nil
}

func latency() { time.Sleep(time.Duration(20+rand.Intn(60)) * time.Millisecond) }

func clone[T proto.Message](in []T) []T {
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = proto.Clone(v).(T)
	}
	return out
}

func (b *Backend) ListAtespaces(context.Context) ([]*ateapi.Atespace, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	return clone(b.atespaces), nil
}

func (b *Backend) ListActors(_ context.Context, atespace string) ([]*ateapi.Actor, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.step()
	var out []*ateapi.Actor
	for _, a := range b.actors {
		if atespace == "" || a.Metadata.Atespace == atespace {
			out = append(out, proto.Clone(a).(*ateapi.Actor))
		}
	}
	return out, nil
}

func (b *Backend) ListActorTemplates(_ context.Context, atespace string) ([]*ateapi.ActorTemplate, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*ateapi.ActorTemplate
	for _, t := range b.templates {
		if atespace == "" || t.Metadata.Atespace == atespace {
			out = append(out, proto.Clone(t).(*ateapi.ActorTemplate))
		}
	}
	return out, nil
}

func (b *Backend) GetActorTemplate(_ context.Context, ref *ateapi.ObjectRef) (*ateapi.ActorTemplate, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.templates {
		if t.Metadata.Atespace == ref.GetAtespace() && t.Metadata.Name == ref.GetName() {
			return proto.Clone(t).(*ateapi.ActorTemplate), nil
		}
	}
	return nil, fmt.Errorf("rpc error: code = NotFound desc = actor template %s/%s not found", ref.GetAtespace(), ref.GetName())
}

func (b *Backend) ListTags(_ context.Context, atespace string) ([]*ateapi.Tag, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*ateapi.Tag
	for _, t := range b.templates {
		if g := t.Status.GetGoldenSnapshotStatus().GetGoldenTag(); g != nil && (atespace == "" || g.Atespace == atespace) {
			out = append(out, &ateapi.Tag{
				Metadata: &ateapi.ResourceMetadata{Atespace: g.Atespace, Name: g.Name, CreateTime: t.Status.GoldenSnapshotStatus.SnapshotTime},
				Scope:    ateapi.TagScope_TAG_SCOPE_ATESPACE,
				Status:   &ateapi.TagStatus{Snapshot: &ateapi.Snapshot{Generation: 1, Fidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY}},
			})
		}
	}
	return out, nil
}

func (b *Backend) ListWorkers(context.Context) ([]*ateapi.Worker, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	return clone(b.workers), nil
}

func (b *Backend) ListWorkerAssignments(_ context.Context, worker string) ([]*ateapi.ActorAssignment, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*ateapi.ActorAssignment
	for _, a := range b.actors {
		if a.Status.GetWorkerAssignment().GetWorker().GetName() == worker {
			out = append(out, &ateapi.ActorAssignment{
				Metadata:  &ateapi.ResourceMetadata{Name: a.Metadata.Name, CreateTime: a.Metadata.UpdateTime},
				Actor:     &ateapi.ObjectRef{Atespace: a.Metadata.Atespace, Name: a.Metadata.Name},
				Resources: &ateapi.Resources{CpuMillicores: 1000, MemoryBytes: 2 << 30},
			})
		}
	}
	return out, nil
}

func (b *Backend) ListWorkerPools(context.Context) ([]infrav1.WorkerPool, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]infrav1.WorkerPool, len(b.pools))
	for i := range b.pools {
		out[i] = *b.pools[i].DeepCopy()
	}
	return out, nil
}

// PodLogs emits JSON lines in the worker's log format for every actor on
// the pod, as a multiplexed worker would.
func (b *Backend) PodLogs(ctx context.Context, _, pod string, follow bool, _ int64) (io.ReadCloser, error) {
	type ref struct{ as, name string }
	b.mu.Lock()
	var on []ref
	for _, a := range b.actors {
		if a.Status.GetWorkerAssignment().GetWorkerPod() == pod {
			on = append(on, ref{a.Metadata.Atespace, a.Metadata.Name})
		}
	}
	b.mu.Unlock()
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		line := func(r ref, container, msg string) {
			c := ""
			if container != "" {
				c = fmt.Sprintf(`,"ate.actor.container.name":%q`, container)
			}
			fmt.Fprintf(pw, `{"time":%q,"message":%q,"labels":{"ate.atespace":%q,"ate.actor.name":%q%s}}`+"\n",
				time.Now().Format(time.RFC3339Nano), msg, r.as, r.name, c)
		}
		for _, r := range on {
			line(r, "", "restored from snapshot generation 2 in 312ms")
			line(r, "agent", "listening on :8080")
		}
		msgs := []string{"tool call: web.search", "tool call: fs.read README.md", "model response 812 tokens", "checkpoint idle timer reset", "GET / 200 4ms"}
		for i := 0; follow && len(on) > 0; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(100+rand.Intn(400)) * time.Millisecond):
			}
			line(on[rand.Intn(len(on))], "agent", msgs[i%len(msgs)])
		}
	}()
	return pr, nil
}
