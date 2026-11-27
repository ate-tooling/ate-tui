package ui

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Backend is the data the UI reads and the writes it can make. ateconn.Conn
// implements it.
type Backend interface {
	ListAtespaces(ctx context.Context) ([]*ateapi.Atespace, error)
	ListActors(ctx context.Context, atespace string) ([]*ateapi.Actor, error)
	ListActorTemplates(ctx context.Context, atespace string) ([]*ateapi.ActorTemplate, error)
	ListTags(ctx context.Context, atespace string) ([]*ateapi.Tag, error)
	ListWorkers(ctx context.Context) ([]*ateapi.Worker, error)
	ListWorkerAssignments(ctx context.Context, worker string) ([]*ateapi.ActorAssignment, error)
	ListWorkerPools(ctx context.Context) ([]infrav1.WorkerPool, error)
	ListSandboxConfigs(ctx context.Context) ([]infrav1.SandboxConfig, error)
	ListCSIDriverConfigs(ctx context.Context) ([]infrav1.CSIDriverConfig, error)
	GetActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error)
	GetActorTemplate(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.ActorTemplate, error)
	GetActorEgressPolicy(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.EgressPolicy, error)
	GetGlobalAccessPolicy(ctx context.Context) (*ateapi.AccessPolicy, error)
	GetAtespaceAccessPolicy(ctx context.Context, atespace string) (*ateapi.AccessPolicy, error)
	PodLogs(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
	PodEvents(ctx context.Context, namespace, pod string) ([]corev1.Event, error)
	// ObjectEvents are the events about any object; an empty namespace
	// looks in every namespace, for a cluster-scoped object.
	ObjectEvents(ctx context.Context, namespace, kind, name string) ([]corev1.Event, error)

	// PrepareProxy mints what DialActor needs and may take a while.
	// DialActor tunnels to an actor port; the router resumes a suspended actor.
	PrepareProxy(ctx context.Context) error
	DialActor(ctx context.Context, ref *ateapi.ObjectRef, port int) (net.Conn, error)

	SuspendActor(ctx context.Context, ref *ateapi.ObjectRef, mode ateapi.DurabilityMode) (*ateapi.Actor, error)
	ResumeActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error)
	RevertActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error)
	DeleteActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error)
	DrainWorker(ctx context.Context, name string) (*ateapi.Worker, error)
	MarkWorkerAvailable(ctx context.Context, name string) (*ateapi.Worker, error)
	MarkWorkerUnavailable(ctx context.Context, name string) (*ateapi.Worker, error)

	CreateAtespace(ctx context.Context, a *ateapi.Atespace) (*ateapi.Atespace, error)
	CreateActorTemplate(ctx context.Context, t *ateapi.ActorTemplate) (*ateapi.ActorTemplate, error)
	CreateActor(ctx context.Context, a *ateapi.Actor) (*ateapi.Actor, error)
	CreateTag(ctx context.Context, t *ateapi.Tag) (*ateapi.Tag, error)

	// Kubernetes writes on a *WorkerPool, *SandboxConfig or *CSIDriverConfig.
	CreateObject(ctx context.Context, obj metav1.Object) (metav1.Object, error)
	UpdateObject(ctx context.Context, obj metav1.Object) (metav1.Object, error)
	DeleteObject(ctx context.Context, obj metav1.Object) error
	ScaleWorkerPool(ctx context.Context, namespace, name string, replicas int32) (*infrav1.WorkerPool, error)
}

type kind string

const (
	kAtespaces   kind = "atespaces"
	kActors      kind = "actors"
	kTemplates   kind = "templates"
	kTags        kind = "tags"
	kWorkers     kind = "workers"
	kWorkerPools kind = "workerpools"
	kSandboxCfgs kind = "sandboxconfigs"
	kCSIDrivers  kind = "csidriverconfigs"
	kAssignments kind = "assignments"
	kHistory     kind = "history"
)

// workerScoped kinds list what one worker holds, scoped by its name.
func workerScoped(k kind) bool { return k == kAssignments || k == kHistory }

// hint is one key shown in the header. Kind and table hints name the
// default key, which the keymap shows as rebound.
type hint struct{ key, desc string }

type kindDef struct {
	kind    kind
	aliases []string
	// scoped kinds are filtered by the current atespace.
	scoped  bool
	columns []string
	// wide columns are appended in wide mode (w); rows carry their cells
	// after the normal ones.
	wide []string
	// drop lists the columns to hide, in order, when the terminal is narrow.
	drop  []int
	hints []hint
	// sortCol is the column a new table sorts by.
	sortCol int
	// manual kinds refresh on r only: their fetch is too heavy to repeat
	// every tick.
	manual bool
	// fetch runs off the UI goroutine and returns ready-to-render rows.
	fetch func(ctx context.Context, b Backend, scope string, now time.Time) ([]row, error)
}

var kinds = []*kindDef{
	{
		kind: kActors, aliases: []string{"actor", "act", "a"}, scoped: true,
		columns: []string{"ATESPACE", "NAME", "STATE", "TEMPLATE", "WORKER", "NODE", "GEN", "UPDATED", "AGE"},
		wide:    []string{"POD", "SANDBOX", "CRASH", "UID"},
		drop:    []int{6, 5, 7, 3},
		hints: []hint{{"enter", "explain"}, {"y", "yaml"}, {"l", "logs"}, {"u", "relations"}, {"e", "egress"},
			{"E", "events"}, {"T", "timeline"}, {"o", "worker"}, {"t", "template"}, {"f", "proxy"}, {"n", "new"}, {"ctrl+n", "clone"}, {" ", "mark"},
			{"s", "suspend"}, {"R", "resume"}, {"v", "revert"}, {"ctrl+d", "delete"}},
		fetch: func(ctx context.Context, b Backend, scope string, now time.Time) ([]row, error) {
			as, err := b.ListActors(ctx, scope)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(as))
			for _, a := range as {
				rows = append(rows, actorRow(a, now))
			}
			return rows, nil
		},
	},
	{
		kind: kWorkers, aliases: []string{"worker", "wk", "w"},
		columns: []string{"NAME", "STATE", "POOL", "NODE", "ACTORS", "CPU", "MEM", "POD", "AGE"},
		wide:    []string{"NAMESPACE", "EPOCHS", "LABELS", "UID"},
		drop:    []int{7, 6, 5, 3},
		hints: []hint{{"enter", "assignments"}, {"y", "yaml"}, {"X", "explain"}, {"u", "relations"},
			{"l", "logs"}, {"E", "events"}, {"p", "pool"}, {" ", "mark"}, {"D", "drain"}, {"A", "available"},
			{"U", "unavailable"}},
		fetch: func(ctx context.Context, b Backend, _ string, now time.Time) ([]row, error) {
			ws, err := b.ListWorkers(ctx)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(ws))
			for _, w := range ws {
				rows = append(rows, workerRow(w, now))
			}
			return rows, nil
		},
	},
	{
		kind: kTemplates, aliases: []string{"template", "actortemplates", "actortemplate", "tpl"}, scoped: true,
		columns: []string{"ATESPACE", "NAME", "IMAGES", "CPU", "MEM", "GOLDEN", "AGE"},
		wide:    []string{"GOLDEN ERROR", "UID"},
		drop:    []int{4, 3, 2},
		hints:   []hint{{"enter", "actors"}, {"y", "yaml"}, {"X", "explain"}, {"u", "relations"}, {"n", "new"}, {"ctrl+n", "clone"}},
		fetch: func(ctx context.Context, b Backend, scope string, now time.Time) ([]row, error) {
			ts, err := b.ListActorTemplates(ctx, scope)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(ts))
			for _, t := range ts {
				rows = append(rows, templateRow(t, now))
			}
			return rows, nil
		},
	},
	{
		kind: kAtespaces, aliases: []string{"atespace", "as", "ns"},
		columns: []string{"NAME", "UID", "AGE"},
		drop:    []int{1},
		hints:   []hint{{"enter", "actors"}, {"y", "yaml"}, {"u", "relations"}, {"n", "new"}},
		fetch: func(ctx context.Context, b Backend, _ string, now time.Time) ([]row, error) {
			as, err := b.ListAtespaces(ctx)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(as))
			for _, a := range as {
				md := a.GetMetadata()
				rows = append(rows, newRow(md.GetName(), a, lvNormal,
					[]string{md.GetName(), md.GetUid(), age(md.GetCreateTime().AsTime(), now)},
					map[int]string{2: sortTime(md.GetCreateTime().AsTime())}))
			}
			return rows, nil
		},
	},
	{
		kind: kTags, aliases: []string{"tag"}, scoped: true,
		columns: []string{"ATESPACE", "NAME", "SCOPE", "SOURCE", "GEN", "FIDELITY", "AGE"},
		wide:    []string{"UID"},
		drop:    []int{5, 3},
		hints:   []hint{{"y", "yaml"}, {"u", "relations"}, {"n", "new"}, {"ctrl+n", "clone"}},
		fetch: func(ctx context.Context, b Backend, scope string, now time.Time) ([]row, error) {
			ts, err := b.ListTags(ctx, scope)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(ts))
			for _, t := range ts {
				md := t.GetMetadata()
				snap := t.GetStatus().GetSnapshot()
				rows = append(rows, newRow(md.GetAtespace()+"/"+md.GetName(), t, lvNormal, []string{
					md.GetAtespace(), md.GetName(),
					explain.TagScope(t.GetScope()),
					dash(t.GetSourceActor().GetName()),
					fmt.Sprint(snap.GetGeneration()),
					explain.SnapshotFidelity(snap.GetFidelity()),
					age(md.GetCreateTime().AsTime(), now),
					md.GetUid(),
				}, map[int]string{6: sortTime(md.GetCreateTime().AsTime())}))
			}
			return rows, nil
		},
	},
	{
		kind: kWorkerPools, aliases: []string{"workerpool", "wp", "pools", "pool"},
		columns: []string{"NAMESPACE", "NAME", "DESIRED", "CURRENT", "READY", "SANDBOX", "CONFIG", "STATUS", "AGE"},
		wide:    []string{"IMAGE", "UID"},
		drop:    []int{3, 2, 6, 5},
		hints: []hint{{"enter", "workers"}, {"y", "yaml"}, {"X", "explain"}, {"u", "relations"}, {"E", "events"},
			{"n", "new"}, {"ctrl+n", "clone"}, {"ctrl+e", "edit"}, {"z", "scale"}, {"ctrl+d", "delete"}},
		fetch: func(ctx context.Context, b Backend, _ string, now time.Time) ([]row, error) {
			ps, err := b.ListWorkerPools(ctx)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(ps))
			for i := range ps {
				rows = append(rows, poolRow(&ps[i], now))
			}
			return rows, nil
		},
	},
	{
		kind: kSandboxCfgs, aliases: []string{"sandboxconfig", "sbc", "sandboxes", "sandbox"},
		columns: []string{"NAME", "CLASS", "DEFAULT", "VERSIONS", "CLASS DEFAULT", "POOLS", "STATUS", "AGE"},
		wide:    []string{"ARCHES", "UID"},
		drop:    []int{4, 3, 5},
		hints: []hint{{"enter", "pools"}, {"y", "yaml"}, {"X", "explain"}, {"u", "relations"}, {"E", "events"},
			{"n", "new"}, {"ctrl+n", "clone"}, {"ctrl+e", "edit"}, {"ctrl+d", "delete"}},
		fetch: fetchSandboxConfigs,
	},
	{
		kind: kCSIDrivers, aliases: []string{"csidriverconfig", "csi", "csidrivers"},
		columns: []string{"NAME", "DRIVER", "ENDPOINT", "TLS", "STATUS", "AGE"},
		wide:    []string{"NODE SOCKET", "UID"},
		drop:    []int{3, 2},
		hints: []hint{{"enter", "yaml"}, {"y", "yaml"}, {"E", "events"},
			{"n", "new"}, {"ctrl+n", "clone"}, {"ctrl+e", "edit"}, {"ctrl+d", "delete"}},
		fetch: func(ctx context.Context, b Backend, _ string, now time.Time) ([]row, error) {
			cs, err := b.ListCSIDriverConfigs(ctx)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(cs))
			for i := range cs {
				rows = append(rows, csiRow(&cs[i], now))
			}
			return rows, nil
		},
	},
	{
		kind: kAssignments, aliases: []string{"assignment"},
		columns: []string{"ATESPACE", "ACTOR", "CPU", "MEM", "AGE"},
		wide:    []string{"UID"},
		hints:   []hint{{"enter", "actor"}, {"y", "yaml"}, {"u", "relations"}},
		fetch: func(ctx context.Context, b Backend, worker string, now time.Time) ([]row, error) {
			if worker == "" {
				return nil, fmt.Errorf("pick a worker and press ⏎ to list its assignments")
			}
			as, err := b.ListWorkerAssignments(ctx, worker)
			if err != nil {
				return nil, err
			}
			rows := make([]row, 0, len(as))
			for _, a := range as {
				md := a.GetMetadata()
				rows = append(rows, newRow(a.GetActor().GetAtespace()+"/"+a.GetActor().GetName(), a, lvNormal, []string{
					a.GetActor().GetAtespace(), a.GetActor().GetName(),
					explain.Millicores(a.GetResources().GetCpuMillicores()),
					explain.Bytes(a.GetResources().GetMemoryBytes()),
					age(md.GetCreateTime().AsTime(), now),
					dash(md.GetUid()),
				}, map[int]string{4: sortTime(md.GetCreateTime().AsTime())}))
			}
			return rows, nil
		},
	},
	{
		kind: kHistory, aliases: []string{"hist"}, manual: true, sortCol: 5,
		columns: []string{"ATESPACE", "ACTOR", "STATUS", "LINES", "FIRST", "LAST"},
		hints:   []hint{{"enter", "logs"}, {"r", "reread"}},
		fetch:   fetchHistory,
	},
}

func lookupKind(name string) *kindDef {
	name = strings.ToLower(name)
	for _, k := range kinds {
		if string(k.kind) == name {
			return k
		}
		for _, a := range k.aliases {
			if a == name {
				return k
			}
		}
	}
	return nil
}

func actorRow(a *ateapi.Actor, now time.Time) row {
	md := a.GetMetadata()
	st := a.GetStatus()
	lv := lvNormal
	switch st.GetState() {
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		lv = lvIdle
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		lv = lvError
	case ateapi.ActorState_ACTOR_STATE_RESUMING, ateapi.ActorState_ACTOR_STATE_SUSPENDING, ateapi.ActorState_ACTOR_STATE_REVERTING:
		lv = lvProgress
		if now.Sub(md.GetUpdateTime().AsTime()) > explain.StuckAfter {
			lv = lvWarn
		}
	case ateapi.ActorState_ACTOR_STATE_DELETING:
		lv = lvGone
	}
	gen := "-"
	if g := st.GetLastAssignedGeneration(); g > 0 {
		gen = fmt.Sprint(g)
	}
	return newRow(md.GetAtespace()+"/"+md.GetName(), a, lv, []string{
		md.GetAtespace(), md.GetName(),
		explain.ActorState(st.GetState()),
		dash(a.GetActorTemplate().GetName()),
		dash(st.GetWorkerAssignment().GetWorker().GetName()),
		dash(st.GetAssignedNode()),
		gen,
		age(md.GetUpdateTime().AsTime(), now),
		age(md.GetCreateTime().AsTime(), now),
		dash(st.GetWorkerAssignment().GetWorkerPod()),
		dash(st.GetWorkerAssignment().GetSandboxRuntime().GetSandboxClass()),
		dash(oneLine(st.GetCrash().GetMessage())),
		dash(md.GetUid()),
	}, map[int]string{
		7: sortTime(md.GetUpdateTime().AsTime()),
		8: sortTime(md.GetCreateTime().AsTime()),
	})
}

func workerRow(w *ateapi.Worker, now time.Time) row {
	md := w.GetMetadata()
	st := w.GetStatus()
	lv := lvNormal
	switch st.GetState() {
	case ateapi.WorkerState_WORKER_STATE_DRAINING:
		lv = lvWarn
	case ateapi.WorkerState_WORKER_STATE_UNAVAILABLE:
		lv = lvError
	}
	capA, used := st.GetCapacity().GetActors(), st.GetAllocated().GetActors()
	if lv == lvNormal && capA > 0 && used >= capA {
		lv = lvWarn
	}
	return newRow(md.GetName(), w, lv, []string{
		md.GetName(),
		explain.WorkerState(st.GetState()),
		dash(w.GetWorkerPool()),
		dash(w.GetNodeName()),
		fmt.Sprintf("%d/%d", used, capA),
		explain.Millicores(st.GetAllocated().GetCpuMillicores()) + "/" + explain.Millicores(st.GetCapacity().GetCpuMillicores()),
		explain.Bytes(st.GetAllocated().GetMemoryBytes()) + "/" + explain.Bytes(st.GetCapacity().GetMemoryBytes()),
		dash(w.GetWorkerPod()),
		age(md.GetCreateTime().AsTime(), now),
		dash(w.GetWorkerNamespace()),
		fmt.Sprintf("%d/%d/%d", st.GetIssuedEpoch(), st.GetRegisteredEpoch(), st.GetObservedEpoch()),
		dash(labelString(w.GetLabels())),
		dash(md.GetUid()),
	}, map[int]string{
		4: fmt.Sprintf("%010d", used),
		8: sortTime(md.GetCreateTime().AsTime()),
	})
}

func templateRow(t *ateapi.ActorTemplate, now time.Time) row {
	md := t.GetMetadata()
	var imgs []string
	for _, c := range t.GetContainers() {
		imgs = append(imgs, c.GetImage())
	}
	gs := t.GetStatus().GetGoldenSnapshotStatus()
	golden, lv := "-", lvNormal
	switch {
	case gs.GetErrorMessage() != "":
		golden, lv = "Error", lvError
	case gs.GetGoldenTag().GetName() != "":
		golden = gs.GetGoldenTag().GetName()
	}
	lim := t.GetResources().GetLimits()
	return newRow(md.GetAtespace()+"/"+md.GetName(), t, lv, []string{
		md.GetAtespace(), md.GetName(), dash(strings.Join(imgs, ",")),
		explain.Millicores(lim.GetCpuMillicores()), explain.Bytes(lim.GetMemoryBytes()),
		golden, age(md.GetCreateTime().AsTime(), now),
		dash(oneLine(gs.GetErrorMessage())), dash(md.GetUid()),
	}, map[int]string{6: sortTime(md.GetCreateTime().AsTime())})
}

func poolRow(p *infrav1.WorkerPool, now time.Time) row {
	lv, st := lvNormal, "Ready"
	if p.Status.ReadyReplicas < p.Spec.Replicas {
		lv, st = lvWarn, "Progressing"
	}
	var classes []string
	for _, sc := range p.Status.SandboxClasses {
		name := string(sc.Name)
		for _, c := range sc.Conditions {
			if c.Type == infrav1.WorkerPoolSandboxClassConditionResolved && c.Status != metav1.ConditionTrue {
				lv, st = lvError, dash(c.Reason)
				name += "!"
			}
		}
		classes = append(classes, name)
	}
	if p.DeletionTimestamp != nil {
		lv, st = lvGone, "Terminating"
	}
	return newRow(p.Namespace+"/"+p.Name, p, lv, []string{
		p.Namespace, p.Name,
		fmt.Sprint(p.Spec.Replicas), fmt.Sprint(p.Status.Replicas), fmt.Sprint(p.Status.ReadyReplicas),
		dash(strings.Join(classes, ",")),
		dash(p.ResolvedSandboxConfig()),
		st,
		age(p.CreationTimestamp.Time, now),
		dash(p.Spec.WorkerImage),
		dash(string(p.UID)),
	}, map[int]string{
		2: fmt.Sprintf("%010d", p.Spec.Replicas),
		3: fmt.Sprintf("%010d", p.Status.Replicas),
		4: fmt.Sprintf("%010d", p.Status.ReadyReplicas),
		8: sortTime(p.CreationTimestamp.Time),
	})
}

func age(t, now time.Time) string { return explain.Age(t, now) }

// sortTime sorts newest first under an ascending sort, like kubectl AGE.
func sortTime(t time.Time) string { return fmt.Sprintf("%020d", 1<<62-t.UnixNano()) }

// oneLine keeps the first line of a message for a table cell.
func oneLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func labelString(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + l[k]
	}
	return strings.Join(keys, ",")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ValidKind reports whether name is a kind or one of its aliases.
func ValidKind(name string) bool { return lookupKind(name) != nil }
