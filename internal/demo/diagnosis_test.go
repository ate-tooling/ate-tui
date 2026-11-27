package demo

import (
	"testing"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func TestMigrateRespectsWorkerState(t *testing.T) {
	// At start+migrateRun+migrateSleep the migrant wants workers[1].
	at := func(b *Backend) time.Time { return b.start.Add(migrateRun + migrateSleep + time.Second) }
	where := func(b *Backend) string { return b.migrant.Status.GetWorkerAssignment().GetWorker().GetName() }
	tests := []struct {
		name  string
		mut   func(w *ateapi.Worker)
		moves bool
	}{
		{"active with room", func(*ateapi.Worker) {}, true},
		{"draining", func(w *ateapi.Worker) { w.Status.State = ateapi.WorkerState_WORKER_STATE_DRAINING }, false},
		{"unavailable", func(w *ateapi.Worker) { w.Status.State = ateapi.WorkerState_WORKER_STATE_UNAVAILABLE }, false},
		{"full", nil, false},
	}
	for _, tt := range tests {
		b := New()
		b.migrate(b.start) // onto workers[0]
		if where(b) != b.workers[0].Metadata.Name {
			t.Fatalf("%s: migrant on %q, want %q", tt.name, where(b), b.workers[0].Metadata.Name)
		}
		if tt.mut != nil {
			tt.mut(b.workers[1])
		} else {
			// Fill workers[1] with real actors, since migrate recounts.
			for _, a := range b.actors {
				if countOn(b, b.workers[1]) >= int(b.workers[1].Status.Capacity.Actors) {
					break
				}
				if a != b.migrant && a.Status.WorkerAssignment == nil && !b.pinned[a] {
					b.place(a, b.workers[1])
				}
			}
			b.recount()
		}
		b.migrate(at(b))
		got := where(b) == b.workers[1].Metadata.Name
		if got != tt.moves {
			t.Errorf("%s: moved to workers[1] = %v, want %v (on %q)", tt.name, got, tt.moves, where(b))
		}
	}
}

func countOn(b *Backend, w *ateapi.Worker) int {
	n := 0
	for _, a := range b.actors {
		if a.Status.GetWorkerAssignment().GetWorker().GetName() == w.Metadata.Name {
			n++
		}
	}
	return n
}
