package demo

import (
	"context"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The writes follow ateapi's documented behavior closely enough to exercise
// the UI: transitions take a poll or two, and invalid ones fail with the
// codes the real server returns.

// settle is how many polls an in-progress transition stays visible.
const settle = 2

func (b *Backend) actor(ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	for _, a := range b.actors {
		if a.Metadata.Atespace == ref.GetAtespace() && a.Metadata.Name == ref.GetName() {
			return a, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "actor %s/%s not found", ref.GetAtespace(), ref.GetName())
}

func (b *Backend) worker(name string) (*ateapi.Worker, error) {
	for _, w := range b.workers {
		if w.Metadata.Name == name {
			return w, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "worker %s not found", name)
}

// begin moves a to an in-progress state that completes after settle polls.
func (b *Backend) begin(a *ateapi.Actor, s ateapi.ActorState) {
	if a == b.migrant {
		b.migrant = nil
	}
	a.Status.State = s
	a.Metadata.UpdateTime = ts(time.Now())
	b.pending[a] = settle
	b.pinned[a] = true
}

// result returns a copy of a written actor. The random lifecycle leaves it
// alone from now on, so only the user's writes move it.
func (b *Backend) result(a *ateapi.Actor) (*ateapi.Actor, error) {
	b.pinned[a] = true
	if a == b.migrant {
		b.migrant = nil
	}
	return proto.Clone(a).(*ateapi.Actor), nil
}

func (b *Backend) busy(a *ateapi.Actor) error {
	if _, ok := b.pending[a]; ok || a.Status.State == ateapi.ActorState_ACTOR_STATE_DELETING {
		return status.Errorf(codes.Aborted, "actor %s/%s is %s; retry when the transition completes",
			a.Metadata.Atespace, a.Metadata.Name, explain.ActorState(a.Status.State))
	}
	return nil
}

func (b *Backend) SuspendActor(_ context.Context, ref *ateapi.ObjectRef, mode ateapi.DurabilityMode) (*ateapi.Actor, error) {
	latency()
	sync := mode == ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS
	if sync {
		// A synchronous suspend returns only once the snapshot is durable.
		time.Sleep(150 * time.Millisecond)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.actor(ref)
	if err != nil {
		return nil, err
	}
	if err := b.busy(a); err != nil {
		return nil, err
	}
	switch a.Status.State {
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		// Returned as it is.
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		if sync {
			b.suspended(a)
			a.Metadata.UpdateTime = ts(time.Now())
			b.recount()
		} else {
			b.begin(a, ateapi.ActorState_ACTOR_STATE_SUSPENDING)
		}
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is %s; only a RUNNING actor can be suspended",
			ref.GetAtespace(), ref.GetName(), explain.ActorState(a.Status.State))
	}
	return b.result(a)
}

func (b *Backend) ResumeActor(_ context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.actor(ref)
	if err != nil {
		return nil, err
	}
	if err := b.busy(a); err != nil {
		return nil, err
	}
	switch a.Status.State {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		// Already running; resumed=false.
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		b.begin(a, ateapi.ActorState_ACTOR_STATE_RESUMING)
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is CRASHED; revert it to its durable snapshot before resuming",
			ref.GetAtespace(), ref.GetName())
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is %s", ref.GetAtespace(), ref.GetName(), explain.ActorState(a.Status.State))
	}
	return b.result(a)
}

func (b *Backend) RevertActor(_ context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.actor(ref)
	if err != nil {
		return nil, err
	}
	if err := b.busy(a); err != nil {
		return nil, err
	}
	switch a.Status.State {
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		a.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
		a.Status.Crash = nil
		a.Status.WorkerAssignment = nil
		a.Metadata.UpdateTime = ts(time.Now())
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		b.begin(a, ateapi.ActorState_ACTOR_STATE_REVERTING)
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is SUSPENDED without a node-local snapshot; it is already at its durable snapshot",
			ref.GetAtespace(), ref.GetName())
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is %s", ref.GetAtespace(), ref.GetName(), explain.ActorState(a.Status.State))
	}
	return b.result(a)
}

func (b *Backend) DeleteActor(_ context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.actor(ref)
	if err != nil {
		return nil, err
	}
	if a.Status.State == ateapi.ActorState_ACTOR_STATE_DELETING {
		return b.result(a)
	}
	b.begin(a, ateapi.ActorState_ACTOR_STATE_DELETING)
	return b.result(a)
}

func (b *Backend) DrainWorker(_ context.Context, name string) (*ateapi.Worker, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	w, err := b.worker(name)
	if err != nil {
		return nil, err
	}
	if w.Status.State != ateapi.WorkerState_WORKER_STATE_DRAINING {
		w.Status.State = ateapi.WorkerState_WORKER_STATE_DRAINING
		w.Metadata.UpdateTime = ts(time.Now())
		// Like ateapi, a drain leaves bound actors alone; it only stops new
		// placements.
	}
	return proto.Clone(w).(*ateapi.Worker), nil
}

func (b *Backend) MarkWorkerAvailable(_ context.Context, name string) (*ateapi.Worker, error) {
	return b.markWorker(name, ateapi.WorkerState_WORKER_STATE_ACTIVE)
}

func (b *Backend) MarkWorkerUnavailable(_ context.Context, name string) (*ateapi.Worker, error) {
	return b.markWorker(name, ateapi.WorkerState_WORKER_STATE_UNAVAILABLE)
}

func (b *Backend) markWorker(name string, s ateapi.WorkerState) (*ateapi.Worker, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	w, err := b.worker(name)
	if err != nil {
		return nil, err
	}
	if w.Status.State == ateapi.WorkerState_WORKER_STATE_DRAINING {
		return nil, status.Errorf(codes.FailedPrecondition, "worker %s is DRAINING; a drain is one-way", name)
	}
	w.Status.State = s
	w.Metadata.UpdateTime = ts(time.Now())
	return proto.Clone(w).(*ateapi.Worker), nil
}
