package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	"github.com/charmbracelet/x/ansi"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func ref(as, name string) *ateapi.ObjectRef { return &ateapi.ObjectRef{Atespace: as, Name: name} }

func actorIn(as, name, tpl string, state ateapi.ActorState, updatedAgo time.Duration) *ateapi.Actor {
	return &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: as, Name: name, UpdateTime: timestamppb.New(now.Add(-updatedAgo))},
		ActorTemplate: ref(as, tpl),
		Status:        &ateapi.ActorStatus{State: state},
	}
}

func workerIn(name, pool string, state ateapi.WorkerState, used, slots int32) *ateapi.Worker {
	return &ateapi.Worker{
		Metadata: &ateapi.ResourceMetadata{Name: name}, WorkerPool: pool, WorkerNamespace: "ate-workers",
		WorkerPod: name + "-pod", NodeName: "node-1",
		Status: &ateapi.WorkerStatus{State: state,
			Capacity:  &ateapi.WorkerResources{Actors: slots, CpuMillicores: 4000, MemoryBytes: 8 << 30},
			Allocated: &ateapi.WorkerResources{Actors: used, CpuMillicores: int64(used) * 1000, MemoryBytes: int64(used) << 30}},
	}
}

func relSummary(rs []relation) string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s>%s:%s", r.rel, r.kind, r.id))
	}
	return strings.Join(out, " ")
}

func TestRelations(t *testing.T) {
	running := actorIn("research", "a1", "tpl", ateapi.ActorState_ACTOR_STATE_RUNNING, 0)
	running.SourceTag = ref("research", "seed")
	running.Status.WorkerAssignment = &ateapi.WorkerAssignment{Worker: ref("", "w1"), WorkerNamespace: "ate-workers",
		WorkerPool: "pool-a", WorkerPod: "w1-pod", NodeName: "node-1"}
	tpl := &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: "research", Name: "tpl"},
		Status: &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{GoldenTag: ref("research", "tpl-golden")}}}
	broken := &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: "research", Name: "tpl"},
		Status: &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{ErrorMessage: "probe timed out"}}}
	suspended := actorIn("research", "a2", "tpl", ateapi.ActorState_ACTOR_STATE_SUSPENDED, 0)
	suspended.Status.AssignedNode = "node-2"
	workers := []*ateapi.Worker{workerIn("w1", "pool-a", ateapi.WorkerState_WORKER_STATE_ACTIVE, 1, 4),
		workerIn("w2", "pool-a", ateapi.WorkerState_WORKER_STATE_ACTIVE, 0, 4), workerIn("w3", "pool-b", ateapi.WorkerState_WORKER_STATE_ACTIVE, 0, 4)}
	pool := &infrav1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Namespace: "ate-workers", Name: "pool-a"},
		Spec: infrav1.WorkerPoolSpec{Replicas: 2}, Status: infrav1.WorkerPoolStatus{ReadyReplicas: 2}}
	assignments := []*ateapi.ActorAssignment{{Actor: ref("research", "a1")}, {Actor: ref("support", "b1")}}

	tests := []struct {
		name string
		got  []relation
		want string
	}{
		{"running actor", actorRelations(running, tpl, workers),
			"template>templates:research/tpl source tag>tags:research/seed golden tag>tags:research/tpl-golden " +
				"worker>workers:w1 pool>workerpools:ate-workers/pool-a node>workers:"},
		{"suspended actor, failed golden", actorRelations(suspended, broken, nil),
			"template>templates:research/tpl golden tag>: node>workers:"},
		{"actor, template not loaded", actorRelations(suspended, nil, nil), "template>templates:research/tpl node>workers:"},
		{"worker", workerRelations(workers[0], assignments, []*infrav1.WorkerPool{pool}),
			"pool>workerpools:ate-workers/pool-a node>workers: assigned actor>actors:research/a1 assigned actor>actors:support/b1"},
		{"template", templateRelations(tpl, []*ateapi.Actor{running, suspended, actorIn("research", "x", "other", 1, 0)}),
			"golden tag>tags:research/tpl-golden actor>actors:research/a1 actor>actors:research/a2"},
		{"pool", poolRelations(pool, workers), "worker>workers:w1 worker>workers:w2"},
	}
	for _, tt := range tests {
		if got := relSummary(tt.got); got != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
	if r := actorRelations(running, tpl, workers)[3]; r.info != "Active  1/4 actors" {
		t.Errorf("worker info %q", r.info)
	}
	if r := actorRelations(suspended, broken, nil)[1]; r.level != lvError || !strings.Contains(r.info, "probe timed out") {
		t.Errorf("golden error relation %+v", r)
	}
}

func TestRelationsJump(t *testing.T) {
	m := demoModel(t, 120, 40)
	selectState(t, m, 2, "Running")
	a := m.selected().obj.(*ateapi.Actor)
	worker := a.GetStatus().GetWorkerAssignment().GetWorker().GetName()
	press(m, "u")
	if m.pager == nil || len(m.pager.items) == 0 {
		t.Fatal("no relations")
	}
	for i, it := range m.pager.items {
		if it.rel == "worker" {
			for j := 0; j < i; j++ {
				press(m, "j")
			}
		}
	}
	press(m, "enter")
	if m.cur.kind != kWorkers || m.selectedID() != worker || m.mode != modeTable {
		t.Fatalf("landed on %s %q, want worker %s", m.cur.kind, m.selectedID(), worker)
	}

	// From the worker to one of its assigned actors, in another atespace scope.
	press(m, "u")
	for i, it := range m.pager.items {
		if it.rel == "assigned actor" {
			m.pager.cursor = i
			press(m, "enter")
			break
		}
	}
	if m.cur.kind != kActors || m.selectedID() == "" || m.atespace == "" {
		t.Fatalf("landed on %s %q in %q", m.cur.kind, m.selectedID(), m.atespace)
	}
	drive(m, m.fetch(m.cur))
	if !strings.HasPrefix(m.selectedID(), m.atespace+"/") {
		t.Fatalf("cursor %q not in %s", m.selectedID(), m.atespace)
	}
}

func TestAggregatePulse(t *testing.T) {
	crashed := func(name string, ago time.Duration) *ateapi.Actor {
		a := actorIn("b", name, "t", ateapi.ActorState_ACTOR_STATE_CRASHED, ago)
		a.Status.Crash = &ateapi.ActorCrash{Message: "oom " + name, CrashTime: timestamppb.New(now.Add(-ago))}
		return a
	}
	actors := []*ateapi.Actor{
		actorIn("a", "r1", "t", ateapi.ActorState_ACTOR_STATE_RUNNING, 0),
		actorIn("a", "r2", "t", ateapi.ActorState_ACTOR_STATE_RUNNING, 0),
		actorIn("a", "s1", "t", ateapi.ActorState_ACTOR_STATE_SUSPENDED, 0),
		actorIn("a", "slow", "t", ateapi.ActorState_ACTOR_STATE_RESUMING, 10*time.Minute),
		actorIn("b", "fresh", "t", ateapi.ActorState_ACTOR_STATE_SUSPENDING, 30*time.Second),
		actorIn("b", "slower", "t", ateapi.ActorState_ACTOR_STATE_DELETING, 20*time.Minute),
		crashed("old", time.Hour), crashed("new", time.Minute),
	}
	workers := []*ateapi.Worker{
		workerIn("w1", "pool-a", ateapi.WorkerState_WORKER_STATE_ACTIVE, 3, 4),
		workerIn("w2", "pool-a", ateapi.WorkerState_WORKER_STATE_DRAINING, 1, 4),
		workerIn("w3", "pool-b", ateapi.WorkerState_WORKER_STATE_ACTIVE, 0, 2),
	}
	pools := []*infrav1.WorkerPool{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pool-b"}, Spec: infrav1.WorkerPoolSpec{Replicas: 3},
			Status: infrav1.WorkerPoolStatus{ReadyReplicas: 1, SandboxClasses: []infrav1.WorkerPoolSandboxClassStatus{{Name: "microvm",
				Conditions: []metav1.Condition{{Type: infrav1.WorkerPoolSandboxClassConditionResolved, Status: metav1.ConditionFalse}}}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pool-a"}, Spec: infrav1.WorkerPoolSpec{Replicas: 2},
			Status: infrav1.WorkerPoolStatus{ReadyReplicas: 2}},
	}
	templates := []*ateapi.ActorTemplate{
		{Metadata: &ateapi.ResourceMetadata{Atespace: "a", Name: "ok"}},
		{Metadata: &ateapi.ResourceMetadata{Atespace: "b", Name: "bad"}, Status: &ateapi.ActorTemplateStatus{
			GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{ErrorMessage: "probe failed"}}},
	}
	d := aggregatePulse(actors, workers, pools, templates, now)

	counts := func(s spaceCount) string {
		var parts []string
		for _, st := range pulseStates {
			if n := s.counts[st]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", explain.ActorState(st), n))
			}
		}
		return fmt.Sprintf("%s %s total=%d", s.name, strings.Join(parts, ","), s.total)
	}
	var got []string
	for _, s := range d.spaces {
		got = append(got, counts(s))
	}
	want := []string{
		"a Running=2,Suspended=1,Resuming=1 total=4",
		"b Suspending=1,Crashed=2,Deleting=1 total=4",
		"all Running=2,Suspended=1,Resuming=1,Suspending=1,Crashed=2,Deleting=1 total=8",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("spaces:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(d.crashes) != 2 || d.crashes[0].id != "b/new" || d.crashes[0].msg != "oom new" {
		t.Errorf("crashes %+v", d.crashes)
	}
	// Only transitions past StuckAfter, longest first.
	if len(d.stuck) != 2 || d.stuck[0].id != "b/slower" || d.stuck[1].id != "a/slow" || d.stuck[1].state != "Resuming" {
		t.Errorf("stuck %+v", d.stuck)
	}
	if len(d.pools) != 2 {
		t.Fatalf("pools %+v", d.pools)
	}
	if a := d.pools[0]; a.name != "pool-a" || a.workers != 2 || a.active != 1 || a.used != 4 || a.slots != 8 || a.cpu != 4000 || a.memCap != 16<<30 {
		t.Errorf("pool-a use %+v", a)
	}
	if len(d.ready) != 2 || d.ready[0].id != "ns/pool-a" || !d.ready[1].unresolved || d.ready[1].ready != 1 {
		t.Errorf("ready %+v", d.ready)
	}
	if len(d.golden) != 1 || d.golden[0].id != "b/bad" {
		t.Errorf("golden %+v", d.golden)
	}

	// One atespace has no "all" row.
	if d := aggregatePulse(actors[:3], nil, nil, nil, now); len(d.spaces) != 1 {
		t.Errorf("single atespace: %d rows", len(d.spaces))
	}
}

func TestPulseView(t *testing.T) {
	m := demoModel(t, 140, 40)
	press(m, ":", "pulse", "enter")
	v := ansi.Strip(m.View())
	for _, want := range []string{"Actors by atespace", "research", "pool-gvisor", "Recent crashes", "OOMKilled",
		"Stuck over 2m", "Golden snapshot errors", "codegen/test-runner"} {
		if !strings.Contains(v, want) {
			t.Errorf("pulse has no %q", want)
		}
	}
	if m.pulse.loading {
		t.Error("pulse still loading after its fetches")
	}
}

func TestRenderEgress(t *testing.T) {
	const secret = "ate-secret://gcp-secret-manager/team/projects/p/secrets/api-key"
	p := &ateapi.EgressPolicy{
		Metadata: &ateapi.ResourceMetadata{Atespace: "research", Name: "default", UpdateTime: timestamppb.New(now.Add(-time.Hour))},
		Rules: []*ateapi.EgressRule{
			{Https: &ateapi.HttpsRule{Hostnames: []string{"api.example.com"}, Effects: &ateapi.HttpRuleEffects{
				ReplaceHeaders: []*ateapi.CredentialHeader{
					{Header: "Authorization", Prefix: "Bearer sk-live-PREFIX", CredentialUri: secret},
					{Header: "X-Actor-Token", ActorJwt: &ateapi.ActorJwtSource{Audiences: []string{"aud-secretish"}, ExpirationSeconds: 600}},
				}}}},
			{Http: &ateapi.HttpRule{Hostnames: []string{"mirror.local"}, Ports: &ateapi.Ports{Numbers: []int32{80, 8080}},
				Effects: &ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{{Header: "X-Key", CredentialUri: secret}}}}},
			{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: []string{"github.com"}, Ports: &ateapi.Ports{All: &ateapi.AllPorts{}}}},
			{Http: &ateapi.HttpRule{Hostnames: []string{"*"}}},
		},
	}
	tests := []struct {
		name    string
		policy  *ateapi.EgressPolicy
		err     error
		want    []string
		mustNot []string
	}{
		{"rules", p, nil,
			[]string{"4 rule(s)", "https", "api.example.com", "port 443 (default)", "Authorization", "X-Actor-Token",
				"actor JWT", "ports 80, 8080", "cleartext HTTP", "tls_passthrough", "all ports", "port 80 (default)", "traffic that matches no rule is refused"},
			[]string{"ate-secret://", "sk-live", "Bearer", "PREFIX", "aud-secretish", "gcp-secret-manager"}},
		{"not found", nil, status.Error(codes.NotFound, "egress policy not found"),
			[]string{"no egress policy: all outbound traffic blocked", "NXDOMAIN"}, nil},
		{"no rules", &ateapi.EgressPolicy{Metadata: &ateapi.ResourceMetadata{Name: "default"}}, nil,
			[]string{"no rules: all outbound traffic blocked"}, nil},
		{"denied", nil, status.Error(codes.PermissionDenied, "nope"), []string{"egress policy: nope"}, []string{"rpc error"}},
	}
	for _, tt := range tests {
		out := ansi.Strip(strings.Join(renderEgress(ref("research", "a1"), tt.policy, tt.err, now), "\n"))
		for _, w := range tt.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: no %q in\n%s", tt.name, w, out)
			}
		}
		for _, w := range tt.mustNot {
			if strings.Contains(out, w) {
				t.Errorf("%s: shows %q", tt.name, w)
			}
		}
	}
}

func TestEgressPagerHidesCredentials(t *testing.T) {
	m := demoModel(t, 200, 50)
	for i := range m.table().visible {
		m.table().cursor = i
		press(m, "e")
		v := m.View()
		if strings.Contains(v, "ate-secret://") || strings.Contains(v, "Bearer") {
			t.Fatalf("egress pager shows a credential:\n%s", v)
		}
		press(m, "esc")
	}
}

func TestRenderAccess(t *testing.T) {
	global := &ateapi.AccessPolicy{Bindings: []*ateapi.Binding{
		{Role: "owner", Members: []string{"user:root@example.com"}},
		{Role: "viewer", Members: []string{"user:a@example.com", "user:b@example.com"}},
	}}
	spaces := []accessSpace{
		{name: "zeta", err: status.Error(codes.NotFound, "x")},
		{name: "alpha", policy: &ateapi.AccessPolicy{Bindings: []*ateapi.Binding{{Role: "editor", Members: []string{"user:ci"}}}}},
		{name: "mid", err: status.Error(codes.PermissionDenied, "denied")},
	}
	out := ansi.Strip(strings.Join(renderAccess(global, nil, spaces), "\n"))
	for _, w := range []string{"no can-i RPC", "owner   user:root@example.com", "viewer  user:a@example.com", "        user:b@example.com",
		"bootstrap owners", "Atespace zeta  no access policy", "editor  user:ci", "Atespace mid  denied"} {
		if !strings.Contains(out, w) {
			t.Errorf("no %q in\n%s", w, out)
		}
	}
	if strings.Index(out, "alpha") > strings.Index(out, "zeta") {
		t.Error("atespaces not sorted")
	}
	out = ansi.Strip(strings.Join(renderAccess(nil, status.Error(codes.NotFound, "x"), nil), "\n"))
	if !strings.Contains(out, "no global access policy yet") {
		t.Errorf("global NotFound:\n%s", out)
	}
}

func event(typ, reason, msg string, ago time.Duration, count int32) corev1.Event {
	return corev1.Event{Type: typ, Reason: reason, Message: msg, Count: count, LastTimestamp: metav1.NewTime(now.Add(-ago))}
}

func TestRenderEvents(t *testing.T) {
	evs := []corev1.Event{
		event(corev1.EventTypeNormal, "Started", "Started container ateworker", 30*time.Hour, 1),
		event(corev1.EventTypeWarning, "Unhealthy", "Readiness probe failed", 4*time.Minute, 3),
		event(corev1.EventTypeNormal, "Pulled", "image present", 31*time.Hour, 1),
	}
	lines := renderEvents(evs, now)
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansi.Strip(l)
	}
	if len(plain) != 4 || !strings.HasPrefix(plain[0], "AGE") {
		t.Fatalf("lines %q", plain)
	}
	if !strings.HasPrefix(plain[1], "4m     Warning  Unhealthy    x3  Readiness probe failed") {
		t.Errorf("newest first, got %q", plain[1])
	}
	if !strings.Contains(plain[3], "Pulled") {
		t.Errorf("oldest last, got %q", plain[3])
	}
	if got := renderEvents(nil, now); !strings.Contains(got[0], "no events") {
		t.Errorf("empty: %q", got)
	}
}

func TestEventsKey(t *testing.T) {
	m := demoModel(t, 160, 40)
	selectState(t, m, 2, "Running")
	press(m, "E")
	if m.pager == nil || !strings.Contains(strings.Join(m.pager.lines, "\n"), "Started") {
		t.Fatal("actor E did not list events")
	}
	press(m, "esc")
	selectState(t, m, 2, "Suspended")
	press(m, "E")
	if m.pager != nil || !m.statusErr || !strings.Contains(m.status, "not on a worker pod") {
		t.Fatalf("suspended actor: pager %v, status %q", m.pager != nil, m.status)
	}
	press(m, ":", "workers", "enter", "E")
	if m.pager == nil || !strings.Contains(m.pager.title, "ate-workers/") {
		t.Fatal("worker E did not open events")
	}
}

// BenchmarkAggregatePulse is one :pulse refresh over 10k actors, off the UI
// goroutine.
func BenchmarkAggregatePulse(b *testing.B) {
	actors := make([]*ateapi.Actor, 10000)
	for i := range actors {
		actors[i] = actorIn(fmt.Sprintf("as-%d", i%20), fmt.Sprintf("a-%d", i), "t", ateapi.ActorState(1+i%7), time.Duration(i)*time.Second)
	}
	workers := make([]*ateapi.Worker, 200)
	for i := range workers {
		workers[i] = workerIn(fmt.Sprintf("w-%d", i), fmt.Sprintf("pool-%d", i%4), ateapi.WorkerState_WORKER_STATE_ACTIVE, 3, 4)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		aggregatePulse(actors, workers, nil, nil, now)
	}
}
