package ui

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	corev1 "k8s.io/api/core/v1"
)

// The history view lists the actors a worker pod's log has lines for. The
// pod keeps every actor's lines, not only the current ones', so an actor
// deleted since can still be read until the pod is recycled or the kubelet
// rotates its log.

// historyTail bounds the lines read for the history and its kept logs.
const historyTail = 100000

// keptActor is one actor's run of lines in a worker pod's log. Lines from
// before the live actor of that name was created belong to an earlier
// actor, which gets its own keptActor.
type keptActor struct {
	ns, pod     string
	ref         *ateapi.ObjectRef
	first, last time.Time
	lines       int
	// actor is the live actor these lines are from, nil once it is gone.
	actor *ateapi.Actor
}

// here reports whether the actor is running on the pod now, so its logs
// can be followed.
func (k *keptActor) here() bool {
	wa := k.actor.GetStatus().GetWorkerAssignment()
	return k.actor.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_RUNNING &&
		wa.GetWorkerNamespace() == k.ns && wa.GetWorkerPod() == k.pod
}

func fetchHistory(ctx context.Context, b Backend, worker string, now time.Time) ([]row, error) {
	if worker == "" {
		return nil, fmt.Errorf("pick a worker and press l to list the actors its pod kept logs for")
	}
	ws, err := b.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	var w *ateapi.Worker
	for _, x := range ws {
		if x.GetMetadata().GetName() == worker {
			w = x
		}
	}
	if w == nil {
		return nil, fmt.Errorf("worker %s not found", worker)
	}
	if w.GetWorkerPod() == "" {
		return nil, fmt.Errorf("worker %s has no pod", worker)
	}
	actors, err := b.ListActors(ctx, "")
	if err != nil {
		return nil, err
	}
	ks, err := readHistory(ctx, b, w.GetWorkerNamespace(), w.GetWorkerPod(), actors)
	if err != nil {
		return nil, err
	}
	rows := make([]row, 0, len(ks))
	for _, k := range ks {
		rows = append(rows, historyRow(k, now))
	}
	return rows, nil
}

// readHistory groups a pod's lines by actor, most recent first.
func readHistory(ctx context.Context, b Backend, ns, pod string, actors []*ateapi.Actor) ([]*keptActor, error) {
	live := map[string]*ateapi.Actor{}
	for _, a := range actors {
		live[a.GetMetadata().GetAtespace()+"/"+a.GetMetadata().GetName()] = a
	}
	tail := int64(historyTail)
	rc, err := b.PodLogs(ctx, ns, pod, &corev1.PodLogOptions{Timestamps: true, TailLines: &tail})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	byID := map[string]*keptActor{}
	var out []*keptActor
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		stamp, line, ok := splitKubeletTimestamp(sc.Bytes())
		if !ok {
			continue
		}
		_, labels, ok := parseWorkerLine(line)
		if !ok {
			continue
		}
		as, _ := labels[labelAtespace].(string)
		name, _ := labels[labelActor].(string)
		if name == "" {
			continue
		}
		id := as + "/" + name
		a := live[id]
		// The kubelet's clock and the control plane's differ a little; a
		// line within that of the create time is the live actor's.
		if a != nil && stamp.Before(a.GetMetadata().GetCreateTime().AsTime().Add(-time.Second)) {
			id, a = id+"#earlier", nil
		}
		k := byID[id]
		if k == nil {
			k = &keptActor{ns: ns, pod: pod, ref: &ateapi.ObjectRef{Atespace: as, Name: name}, first: stamp, actor: a}
			byID[id] = k
			out = append(out, k)
		}
		k.last = stamp
		k.lines++
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].last.After(out[j].last) })
	return out, nil
}

func historyRow(k *keptActor, now time.Time) row {
	status, lv := "Deleted", lvGone
	if k.actor != nil {
		status, lv = explain.ActorState(k.actor.GetStatus().GetState()), lvNormal
		if pod := k.actor.GetStatus().GetWorkerAssignment().GetWorkerPod(); pod != k.pod {
			lv = lvIdle
			if pod != "" {
				status += " elsewhere"
			}
		}
	}
	id := k.ref.GetAtespace() + "/" + k.ref.GetName()
	if k.actor == nil {
		id += "@" + k.first.UTC().Format(time.RFC3339)
	}
	return newRow(id, k, lv, []string{
		k.ref.GetAtespace(), k.ref.GetName(), status, fmt.Sprint(k.lines),
		age(k.first, now), age(k.last, now),
	}, map[int]string{
		3: fmt.Sprintf("%012d", k.lines),
		4: sortTime(k.first),
		5: sortTime(k.last),
	})
}

// openKept shows the lines a pod kept for an actor. An actor running there
// now is followed instead, as l on the actor would.
func (m *Model) openKept(k *keptActor) tea.Cmd {
	if k.here() {
		return m.openLogs(k.actor)
	}
	id := k.ref.GetAtespace() + "/" + k.ref.GetName()
	p := m.newPager(stTitleKind.Render("logs") + " " + stTitleCount.Render(id))
	p.follow, p.loading, p.lines = false, false, nil
	m.logs = &logStream{gen: p.gen, ref: k.ref, kept: true, lastPod: k.ns + "/" + k.pod}
	if k.actor == nil {
		m.appendLogs([]logLine{notice("actor deleted; these lines are what " + k.ns + "/" + k.pod + " kept")})
	}
	tail := int64(historyTail)
	opts := &corev1.PodLogOptions{Timestamps: true, TailLines: &tail}
	return m.runStream(k.ns, k.pod, opts, k.first.Add(-time.Nanosecond), k.last)
}
