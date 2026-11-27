// Package explain turns substrate objects into a short diagnosis: what state
// something is in, why, and what to look at next. It runs on data already
// fetched from ateapi and needs no other service.
package explain

import (
	"fmt"
	"sort"
	"strings"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Level ranks a finding.
type Level int

const (
	OK Level = iota
	Info
	Warn
	Error
)

// Finding is one line of diagnosis with optional supporting detail.
type Finding struct {
	Level  Level
	Title  string
	Detail []string
}

// Fact is a labeled value shown above the findings.
type Fact struct {
	Label, Value string
}

// Report is the result of explaining one object.
type Report struct {
	Subject  string
	Summary  string
	Level    Level
	Facts    []Fact
	Findings []Finding
	// Next lists commands that help dig further.
	Next []string
}

func (r *Report) add(l Level, title string, detail ...string) {
	r.Findings = append(r.Findings, Finding{Level: l, Title: title, Detail: detail})
	if l > r.Level {
		r.Level = l
	}
}

func (r *Report) fact(label, value string) {
	if value != "" {
		r.Facts = append(r.Facts, Fact{label, value})
	}
}

// StuckAfter is how long an actor may sit in a transitional state before it
// is reported as stuck.
const StuckAfter = 2 * time.Minute

// ActorInput is what Actor needs. Template and Workers may be nil when they
// could not be fetched; the report says so instead of guessing.
type ActorInput struct {
	Actor       *ateapi.Actor
	Template    *ateapi.ActorTemplate
	TemplateErr error
	Workers     []*ateapi.Worker
	// PodEvents are the Kubernetes events of the worker pod hosting the
	// actor; nil when not fetched.
	PodEvents []corev1.Event
	Now       time.Time
}

// Actor explains an actor's state.
func Actor(in ActorInput) Report {
	a := in.Actor
	md := a.GetMetadata()
	st := a.GetStatus()
	state := ActorState(st.GetState())
	r := Report{Subject: fmt.Sprintf("actor %s/%s", md.GetAtespace(), md.GetName())}

	r.fact("State", state)
	r.fact("Template", refString(a.GetActorTemplate()))
	if t := a.GetSourceTag(); t.GetName() != "" {
		r.fact("Source tag", refString(t))
	}
	r.fact("Node", st.GetAssignedNode())
	if wa := st.GetWorkerAssignment(); wa != nil {
		r.fact("Worker", wa.GetWorker().GetName())
		r.fact("Pod", wa.GetWorkerNamespace()+"/"+wa.GetWorkerPod())
		r.fact("Pool", wa.GetWorkerPool())
		r.fact("Runtime", runtimeString(wa.GetSandboxRuntime()))
	}
	r.fact("Updated", ago(md.GetUpdateTime(), in.Now))
	r.fact("Created", ago(md.GetCreateTime(), in.Now))

	// sinceUpdate is negative when update_time is unset, so stuck says
	// nothing about how long the transition has lasted.
	sinceUpdate := time.Duration(-1)
	if u := md.GetUpdateTime(); u != nil && u.AsTime().Unix() > 0 {
		sinceUpdate = in.Now.Sub(u.AsTime())
	}
	ref := fmt.Sprintf("%s -a %s", md.GetName(), md.GetAtespace())

	switch st.GetState() {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		r.Summary = "Actor is running."
		r.add(OK, "Running on worker "+st.GetWorkerAssignment().GetWorker().GetName())
		checkAssignedWorker(&r, st.GetWorkerAssignment(), in.Workers)
		checkPodEvents(&r, st.GetWorkerAssignment().GetWorkerNamespace(), st.GetWorkerAssignment().GetWorkerPod(), in.PodEvents, in.Now)
		r.Next = append(r.Next, "kubectl ate logs actor "+ref, "kubectl ate proxy actor "+ref)
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		r.Summary = "Actor is suspended and resumes on the next request."
		r.add(OK, "Suspended")
		checkSnapshots(&r, st)
		checkPlacement(&r, a, in.Template, in.Workers)
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		r.Summary = "Actor crashed and will not resume until reverted."
		c := st.GetCrash()
		detail := []string{}
		if c.GetCrashTime() != nil {
			detail = append(detail, "crashed "+Age(c.GetCrashTime().AsTime(), in.Now)+" ago")
		}
		r.add(Error, "Crashed: "+orDash(c.GetMessage()), detail...)
		checkSnapshots(&r, st)
		r.Next = append(r.Next,
			"kubectl ate logs actor "+ref+" --source=lifecycle",
			"kubectl ate revert actor "+ref)
	case ateapi.ActorState_ACTOR_STATE_RESUMING:
		r.Summary = "Actor is resuming onto a worker."
		stuck(&r, state, sinceUpdate)
		checkAssignedWorker(&r, st.GetWorkerAssignment(), in.Workers)
		checkPlacement(&r, a, in.Template, in.Workers)
		r.Next = append(r.Next, "kubectl ate logs actor "+ref+" --source=lifecycle")
	case ateapi.ActorState_ACTOR_STATE_SUSPENDING:
		r.Summary = "Actor is writing a snapshot before it leaves its worker."
		stuck(&r, state, sinceUpdate)
		checkAssignedWorker(&r, st.GetWorkerAssignment(), in.Workers)
		checkSnapshots(&r, st)
	case ateapi.ActorState_ACTOR_STATE_REVERTING:
		r.Summary = "Actor is reverting to its last good snapshot."
		stuck(&r, state, sinceUpdate)
	case ateapi.ActorState_ACTOR_STATE_DELETING:
		r.Summary = "Actor is being deleted."
		stuck(&r, state, sinceUpdate)
	default:
		r.Summary = "Actor state is unknown to this client."
		r.add(Warn, "Unrecognized state "+st.GetState().String())
	}

	checkTemplate(&r, a, in.Template, in.TemplateErr)
	return r
}

func stuck(r *Report, state string, since time.Duration) {
	if since < 0 {
		r.add(Info, state, "the actor has no update time, so how long it has been "+strings.ToLower(state)+" is unknown")
		return
	}
	if since > StuckAfter {
		r.add(Warn, fmt.Sprintf("%s for %s", state, roundDur(since)),
			fmt.Sprintf("transitions normally finish in seconds; anything over %s is unusual", StuckAfter))
	} else {
		r.add(Info, fmt.Sprintf("%s for %s", state, roundDur(since)))
	}
}

func checkAssignedWorker(r *Report, wa *ateapi.WorkerAssignment, workers []*ateapi.Worker) {
	if wa == nil || workers == nil {
		return
	}
	name := wa.GetWorker().GetName()
	w := findWorker(workers, name)
	if w == nil {
		r.add(Error, "Assigned worker "+name+" is not registered",
			"the worker pod may have been deleted; check pod "+wa.GetWorkerNamespace()+"/"+wa.GetWorkerPod())
		return
	}
	switch w.GetStatus().GetState() {
	case ateapi.WorkerState_WORKER_STATE_DRAINING:
		r.add(Warn, "Worker "+name+" is draining", "no new actors are placed there; this actor stays until it suspends")
	case ateapi.WorkerState_WORKER_STATE_UNAVAILABLE:
		r.add(Error, "Worker "+name+" is unavailable")
	}
	if wa.GetWorkerPod() != "" && w.GetWorkerPod() != "" && wa.GetWorkerPod() != w.GetWorkerPod() {
		r.add(Warn, "Assignment points at pod "+wa.GetWorkerPod()+" but worker "+name+" now runs in "+w.GetWorkerPod())
	}
}

func checkSnapshots(r *Report, st *ateapi.ActorStatus) {
	snaps := st.GetSnapshots()
	if len(snaps) == 0 {
		if st.GetState() == ateapi.ActorState_ACTOR_STATE_SUSPENDED {
			r.add(Info, "No snapshots", "the actor resumes from its template")
		}
		return
	}
	// Sort a copy: st may be a cached object other goroutines read.
	snaps = append([]*ateapi.Snapshot(nil), snaps...)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].GetGeneration() > snaps[j].GetGeneration() })
	var lines []string
	durable := false
	for _, s := range snaps {
		ds := s.GetDurableSnapshot().GetState()
		if ds == ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
			durable = true
		}
		lines = append(lines, fmt.Sprintf("gen %d  %s  durable=%s  locality=%s",
			s.GetGeneration(), SnapshotFidelity(s.GetFidelity()),
			title(trimEnum(ds.String(), "SNAPSHOT_STORAGE_STATE_")), orDash(s.GetLocality())))
	}
	r.add(Info, fmt.Sprintf("%d snapshot generation(s)", len(snaps)), lines...)
	if !durable && st.GetAssignedNode() != "" {
		r.add(Warn, "No durable snapshot yet",
			"resume depends on the local snapshot on node "+st.GetAssignedNode(),
			"if that node goes away the actor loses state since its last durable snapshot")
	}
}

// checkPlacement looks for a worker the actor could resume on.
func checkPlacement(r *Report, a *ateapi.Actor, t *ateapi.ActorTemplate, workers []*ateapi.Worker) {
	if workers == nil {
		return
	}
	sel := a.GetWorkerSelector().GetMatchLabels()
	if len(sel) == 0 {
		sel = t.GetWorkerSelector().GetMatchLabels()
	}
	var matching, active, free int
	for _, w := range workers {
		if !labelsMatch(sel, w.GetLabels()) {
			continue
		}
		matching++
		if w.GetStatus().GetState() != ateapi.WorkerState_WORKER_STATE_ACTIVE {
			continue
		}
		active++
		if hasFreeSlot(w) {
			free++
		}
	}
	selStr := "any worker"
	if len(sel) > 0 {
		selStr = "selector " + labelsString(sel)
	}
	switch {
	case matching == 0:
		r.add(Error, "No worker matches "+selStr, "the actor cannot resume until a matching worker registers")
	case active == 0:
		r.add(Error, fmt.Sprintf("%d worker(s) match %s but none is active", matching, selStr))
	case free == 0:
		r.add(Warn, fmt.Sprintf("All %d active worker(s) matching %s are full", active, selStr),
			"resume waits for a slot; scale the WorkerPool or suspend idle actors")
	default:
		r.add(OK, fmt.Sprintf("%d of %d matching worker(s) have a free slot", free, matching))
	}
}

func checkTemplate(r *Report, a *ateapi.Actor, t *ateapi.ActorTemplate, err error) {
	if err != nil {
		r.add(Error, "Template "+refString(a.GetActorTemplate())+" could not be loaded", err.Error())
		return
	}
	if t == nil {
		return
	}
	if msg := t.GetStatus().GetGoldenSnapshotStatus().GetErrorMessage(); msg != "" {
		r.add(Warn, "Template golden snapshot failed", msg)
	}
}

// WorkerInput is what Worker needs.
type WorkerInput struct {
	Worker      *ateapi.Worker
	Assignments []*ateapi.ActorAssignment
	PodEvents   []corev1.Event
	Now         time.Time
}

// Worker explains a worker's state and load.
func Worker(in WorkerInput) Report {
	w := in.Worker
	st := w.GetStatus()
	r := Report{Subject: "worker " + w.GetMetadata().GetName()}
	r.fact("State", WorkerState(st.GetState()))
	r.fact("Pod", w.GetWorkerNamespace()+"/"+w.GetWorkerPod())
	r.fact("Pool", w.GetWorkerPool())
	r.fact("Node", w.GetNodeName())
	r.fact("Actors", fmt.Sprintf("%d / %d", st.GetAllocated().GetActors(), st.GetCapacity().GetActors()))
	r.fact("CPU", fmt.Sprintf("%s / %s", Millicores(st.GetAllocated().GetCpuMillicores()), Millicores(st.GetCapacity().GetCpuMillicores())))
	r.fact("Memory", fmt.Sprintf("%s / %s", Bytes(st.GetAllocated().GetMemoryBytes()), Bytes(st.GetCapacity().GetMemoryBytes())))
	r.fact("Epoch", fmt.Sprintf("issued %d  registered %d  observed %d", st.GetIssuedEpoch(), st.GetRegisteredEpoch(), st.GetObservedEpoch()))
	if len(w.GetLabels()) > 0 {
		r.fact("Labels", labelsString(w.GetLabels()))
	}

	switch st.GetState() {
	case ateapi.WorkerState_WORKER_STATE_ACTIVE:
		r.Summary = "Worker is accepting actors."
		r.add(OK, "Active")
	case ateapi.WorkerState_WORKER_STATE_DRAINING:
		r.Summary = "Worker is draining and accepts no new actors."
		r.add(Warn, "Draining", "no new actors are placed; bound actors stay until they suspend")
	case ateapi.WorkerState_WORKER_STATE_UNAVAILABLE:
		r.Summary = "Worker is unavailable."
		r.add(Error, "Unavailable", "the scheduler places no actors here")
	default:
		r.Summary = "Worker state is unknown."
		r.add(Warn, "State "+st.GetState().String())
	}

	if c := st.GetCapacity(); c.GetActors() > 0 {
		pct := 100 * int(st.GetAllocated().GetActors()) / int(c.GetActors())
		switch {
		case pct >= 100:
			r.add(Warn, "Full: no free actor slots")
		case pct >= 80:
			r.add(Info, fmt.Sprintf("%d%% of actor slots in use", pct))
		}
	}
	if st.GetRegisteredEpoch() != 0 && st.GetObservedEpoch() < st.GetRegisteredEpoch() {
		r.add(Warn, "Worker has not observed its registered epoch",
			fmt.Sprintf("observed %d, registered %d", st.GetObservedEpoch(), st.GetRegisteredEpoch()))
	}

	if in.Assignments != nil {
		lines := make([]string, 0, len(in.Assignments))
		for _, as := range in.Assignments {
			lines = append(lines, fmt.Sprintf("%s  cpu=%s mem=%s", refString(as.GetActor()),
				Millicores(as.GetResources().GetCpuMillicores()), Bytes(as.GetResources().GetMemoryBytes())))
		}
		r.add(Info, fmt.Sprintf("%d actor assignment(s)", len(in.Assignments)), lines...)
	}
	checkPodEvents(&r, w.GetWorkerNamespace(), w.GetWorkerPod(), in.PodEvents, in.Now)
	r.Next = append(r.Next, "kubectl -n "+w.GetWorkerNamespace()+" describe pod "+w.GetWorkerPod())
	return r
}

// TemplateInput is what Template needs.
type TemplateInput struct {
	Template *ateapi.ActorTemplate
	Actors   []*ateapi.Actor
	Now      time.Time
}

// Template explains a template and the actors built from it.
func Template(in TemplateInput) Report {
	t := in.Template
	md := t.GetMetadata()
	r := Report{Subject: fmt.Sprintf("template %s/%s", md.GetAtespace(), md.GetName())}
	for _, c := range t.GetContainers() {
		r.fact("Container "+c.GetName(), c.GetImage())
	}
	if l := t.GetResources().GetLimits(); l != nil {
		r.fact("Limits", fmt.Sprintf("cpu=%s mem=%s", Millicores(l.GetCpuMillicores()), Bytes(l.GetMemoryBytes())))
	}
	if sel := t.GetWorkerSelector().GetMatchLabels(); len(sel) > 0 {
		r.fact("Worker selector", labelsString(sel))
	}

	gs := t.GetStatus().GetGoldenSnapshotStatus()
	switch {
	case gs.GetErrorMessage() != "":
		r.Summary = "Golden snapshot failed; new actors start cold."
		r.add(Error, "Golden snapshot failed", gs.GetErrorMessage())
	case gs.GetGoldenTag().GetName() != "":
		r.Summary = "Template has a golden snapshot."
		r.add(OK, "Golden snapshot "+refString(gs.GetGoldenTag()), "taken "+Age(gs.GetSnapshotTime().AsTime(), in.Now)+" ago")
	default:
		r.Summary = "Template has no golden snapshot."
		r.add(Info, "No golden snapshot")
	}

	counts := map[string]int{}
	n := 0
	for _, a := range in.Actors {
		if a.GetActorTemplate().GetName() != md.GetName() || a.GetActorTemplate().GetAtespace() != md.GetAtespace() {
			continue
		}
		n++
		counts[ActorState(a.GetStatus().GetState())]++
	}
	if n > 0 {
		var parts []string
		for _, s := range sortedKeys(counts) {
			parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
		}
		l := Info
		if counts["Crashed"] > 0 {
			l = Warn
		}
		r.add(l, fmt.Sprintf("%d actor(s) use this template", n), strings.Join(parts, "  "))
	}
	return r
}

// WorkerPool explains a WorkerPool's rollout and sandbox config resolution.
func WorkerPool(p *infrav1.WorkerPool, workers []*ateapi.Worker, now time.Time) Report {
	r := Report{Subject: fmt.Sprintf("workerpool %s/%s", p.Namespace, p.Name)}
	r.fact("Replicas", fmt.Sprintf("%d desired  %d current  %d ready", p.Spec.Replicas, p.Status.Replicas, p.Status.ReadyReplicas))
	r.fact("Image", p.Spec.WorkerImage)
	r.fact("Age", Age(p.CreationTimestamp.Time, now))

	switch {
	case p.Status.ReadyReplicas < p.Spec.Replicas:
		r.Summary = "WorkerPool is not fully ready."
		r.add(Warn, fmt.Sprintf("%d of %d replicas ready", p.Status.ReadyReplicas, p.Spec.Replicas),
			"check the worker pods: kubectl -n "+p.Namespace+" get pods -l "+orDash(p.Status.Selector))
	default:
		r.Summary = "WorkerPool is ready."
		r.add(OK, fmt.Sprintf("%d/%d replicas ready", p.Status.ReadyReplicas, p.Spec.Replicas))
	}

	for _, sc := range p.Status.SandboxClasses {
		for _, c := range sc.Conditions {
			if c.Type != infrav1.WorkerPoolSandboxClassConditionResolved {
				continue
			}
			if c.Status == metav1.ConditionTrue {
				cfg := ""
				if sc.ConfigRef != nil {
					cfg = " using " + sc.ConfigRef.Name
				}
				r.add(OK, fmt.Sprintf("Sandbox class %s resolved%s", sc.Name, cfg))
			} else {
				r.add(Error, fmt.Sprintf("Sandbox class %s not resolved: %s", sc.Name, c.Reason), c.Message)
				if r.Summary == "WorkerPool is ready." {
					r.Summary = "WorkerPool has an unresolved sandbox class."
				}
			}
		}
		if sc.PendingConfigRef != nil {
			r.add(Info, fmt.Sprintf("Sandbox class %s is rolling to %s", sc.Name, sc.PendingConfigRef.Name))
		}
	}

	if workers != nil {
		states := map[string]int{}
		n := 0
		for _, w := range workers {
			if w.GetWorkerPool() == p.Name && w.GetWorkerNamespace() == p.Namespace {
				n++
				states[WorkerState(w.GetStatus().GetState())]++
			}
		}
		var parts []string
		for _, s := range sortedKeys(states) {
			parts = append(parts, fmt.Sprintf("%s=%d", s, states[s]))
		}
		l := Info
		if int32(n) < p.Status.ReadyReplicas {
			l = Warn
			parts = append(parts, "fewer registered workers than ready pods")
		}
		r.add(l, fmt.Sprintf("%d registered worker(s)", n), strings.Join(parts, "  "))
	}
	return r
}

// SandboxConfig explains a SandboxConfig's versions, the pools that use it
// and what holds up its deletion. pools nil means they are not known.
func SandboxConfig(c *infrav1.SandboxConfig, pools []*infrav1.WorkerPool, now time.Time) Report {
	r := Report{Subject: "sandboxconfig " + c.Name}
	r.fact("Class", string(c.Spec.SandboxClass))
	r.fact("Default version", c.Spec.DefaultVersion)
	var vs []string
	for _, v := range c.Spec.Versions {
		if v.State != nil && *v.State == infrav1.SandboxVersionStateDisabled {
			vs = append(vs, v.Name+" (disabled)")
		} else {
			vs = append(vs, v.Name)
		}
	}
	r.fact("Versions", strings.Join(vs, ", "))
	classDefault := c.Annotations[infrav1.SandboxConfigClassDefaultAnnotation] == "true"
	if classDefault {
		r.fact("Class default", "yes: pools of class "+string(c.Spec.SandboxClass)+" without a configRef may use it (the newest default wins)")
	}
	r.fact("Age", Age(c.CreationTimestamp.Time, now))
	r.Next = []string{"kubectl get sandboxconfig " + c.Name + " -o yaml"}

	if def, ok := c.Spec.DefaultVersionConfig(); ok {
		var arches []string
		for a := range def.Assets {
			arches = append(arches, a)
		}
		sort.Strings(arches)
		r.add(OK, "Default version "+def.Name+" is enabled", "architectures: "+orDash(strings.Join(arches, ", ")))
	} else {
		r.add(Error, "defaultVersion "+orDash(c.Spec.DefaultVersion)+" names no enabled version",
			"new sandboxes of pools using this config have nothing to boot")
	}

	var using, switching, waiting []string
	for _, p := range pools {
		id := p.Namespace + "/" + p.Name
		named := false
		for _, sc := range p.Spec.SandboxClasses {
			if sc.ConfigRef != nil && sc.ConfigRef.Name == c.Name {
				named = true
			}
		}
		inStatus := false
		for _, sc := range p.Status.SandboxClasses {
			if sc.ConfigRef != nil && sc.ConfigRef.Name == c.Name {
				using, inStatus = append(using, id), true
			}
			if sc.PendingConfigRef != nil && sc.PendingConfigRef.Name == c.Name {
				switching, inStatus = append(switching, id), true
			}
		}
		if named && !inStatus {
			waiting = append(waiting, id)
		}
	}
	switch {
	case pools == nil:
		r.add(Info, "WorkerPools could not be listed; which pools use this config is unknown")
	case len(using) > 0:
		r.add(Info, fmt.Sprintf("%d pool(s) use this config", len(using)), using...)
	default:
		r.add(Info, "No pool uses this config")
	}
	if len(switching) > 0 {
		r.add(Info, fmt.Sprintf("%d pool(s) are switching to this config", len(switching)), switching...)
	}
	if len(waiting) > 0 {
		r.add(Warn, fmt.Sprintf("%d pool(s) name this config but have not resolved it", len(waiting)),
			append(waiting, "press X on the pool for the reason")...)
	}

	held := len(using) + len(switching)
	switch {
	case c.DeletionTimestamp != nil && held > 0:
		r.Summary = "SandboxConfig is being deleted, but pools still use it."
		r.add(Warn, fmt.Sprintf("Deleting since %s ago; %d pool(s) hold it", Age(c.DeletionTimestamp.Time, now), held),
			"point those pools at another SandboxConfig (spec.sandboxClasses[].configRef); the controller then releases it")
	case c.DeletionTimestamp != nil && pools == nil && hasFinalizer(c.Finalizers, infrav1.SandboxConfigWorkerPoolProtectionFinalizer):
		r.Summary = "SandboxConfig is being deleted."
		r.add(Warn, fmt.Sprintf("Deleting since %s ago; its finalizer stays while pools use it", Age(c.DeletionTimestamp.Time, now)),
			"WorkerPools could not be listed, so whether pools still use it is unknown",
			"check spec.sandboxClasses[].configRef on the WorkerPools with an account that can list them")
	case c.DeletionTimestamp != nil && hasFinalizer(c.Finalizers, infrav1.SandboxConfigWorkerPoolProtectionFinalizer):
		r.Summary = "SandboxConfig is being deleted."
		r.add(Warn, fmt.Sprintf("Deleting since %s ago; waiting for atecontroller to remove its finalizer", Age(c.DeletionTimestamp.Time, now)),
			"if this lasts, check that atecontroller is running in ate-system")
	case c.DeletionTimestamp != nil:
		r.Summary = "SandboxConfig is being deleted."
	case r.Level == Error:
		r.Summary = "SandboxConfig has no usable default version."
	case len(using) > 0:
		r.Summary = fmt.Sprintf("SandboxConfig is in use by %d pool(s).", len(using))
	default:
		r.Summary = "SandboxConfig is not in use."
	}
	return r
}

func hasFinalizer(fs []string, f string) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}

// RecentEvents is how far back Warning events on a worker pod count.
const RecentEvents = time.Hour

// checkPodEvents reports recent Warning events on a worker pod, newest
// first.
func checkPodEvents(r *Report, namespace, pod string, events []corev1.Event, now time.Time) {
	var warn []corev1.Event
	for _, e := range events {
		if e.Type == corev1.EventTypeWarning && now.Sub(EventTime(&e)) <= RecentEvents {
			warn = append(warn, e)
		}
	}
	if len(warn) == 0 {
		return
	}
	sort.SliceStable(warn, func(i, j int) bool { return EventTime(&warn[i]).After(EventTime(&warn[j])) })
	var lines []string
	for i, e := range warn {
		if i == 5 {
			lines = append(lines, fmt.Sprintf("… and %d more", len(warn)-i))
			break
		}
		count := ""
		if n := EventCount(&e); n > 1 {
			count = fmt.Sprintf(" x%d", n)
		}
		lines = append(lines, fmt.Sprintf("%s%s, %s ago: %s", e.Reason, count, Age(EventTime(&e), now), e.Message))
	}
	r.add(Warn, fmt.Sprintf("%d Warning event(s) on worker pod %s/%s in the last hour", len(warn), namespace, pod), lines...)
	r.Next = append(r.Next, "kubectl -n "+namespace+" get events --field-selector involvedObject.name="+pod)
}

// EventTime is when an event last happened, from whichever field the
// reporter set. events.k8s.io recorders leave LastTimestamp zero, keep
// EventTime at the first occurrence and record repeats in Series.
func EventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// EventCount is how many times an event happened: Count from core/v1
// recorders or Series.Count from events.k8s.io ones, whichever is larger.
func EventCount(e *corev1.Event) int32 {
	n := e.Count
	if e.Series != nil && e.Series.Count > n {
		n = e.Series.Count
	}
	return n
}

func findWorker(ws []*ateapi.Worker, name string) *ateapi.Worker {
	for _, w := range ws {
		if w.GetMetadata().GetName() == name {
			return w
		}
	}
	return nil
}

func hasFreeSlot(w *ateapi.Worker) bool {
	c := w.GetStatus().GetCapacity().GetActors()
	return c == 0 || w.GetStatus().GetAllocated().GetActors() < c
}

func labelsMatch(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
