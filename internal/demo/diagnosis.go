package demo

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The migrant runs on one worker, suspends, runs on another, and suspends
// again, on this cycle.
const (
	migrateRun   = 12 * time.Second
	migrateSleep = 4 * time.Second
)

// migrate moves the migrant to the phase of its cycle that now falls in.
func (b *Backend) migrate(now time.Time) {
	a := b.migrant
	if a == nil || len(b.workers) < 2 {
		return
	}
	if _, busy := b.pending[a]; busy {
		return
	}
	cycle := 2 * (migrateRun + migrateSleep)
	t := now.Sub(b.start) % cycle
	var want *ateapi.Worker
	switch {
	case t < migrateRun:
		want = b.workers[0]
	case t >= migrateRun+migrateSleep && t < 2*migrateRun+migrateSleep:
		want = b.workers[1]
	}
	cur := a.Status.GetWorkerAssignment().GetWorker().GetName()
	switch {
	case want == nil && a.Status.State == ateapi.ActorState_ACTOR_STATE_RUNNING:
		b.suspended(a)
		a.Metadata.UpdateTime = ts(now)
	case want != nil && cur != want.Metadata.Name:
		if a.Status.State == ateapi.ActorState_ACTOR_STATE_RUNNING {
			b.suspended(a)
		}
		b.place(a, want)
	default:
		return
	}
	b.recount()
}

func (b *Backend) GetActor(_ context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.migrate(time.Now())
	a, err := b.actor(ref)
	if err != nil {
		return nil, err
	}
	return proto.Clone(a).(*ateapi.Actor), nil
}

// credential is how the demo policies reference secrets. ate-tui must never
// show it.
func credential(name string) string {
	return "ate-secret://gcp-secret-manager/demo/projects/demo/secrets/" + name + "/versions/latest"
}

func hosts(h ...string) []string { return h }

func ports(n ...int32) *ateapi.Ports {
	if len(n) == 0 {
		return &ateapi.Ports{All: &ateapi.AllPorts{}}
	}
	return &ateapi.Ports{Numbers: n}
}

// egressRules are the demo policies, by template.
var egressRules = map[string][]*ateapi.EgressRule{
	"browser-agent": {
		{Https: &ateapi.HttpsRule{Hostnames: hosts("api.search.example.com"), Effects: &ateapi.HttpRuleEffects{
			ReplaceHeaders: []*ateapi.CredentialHeader{{Header: "Authorization", Prefix: "Bearer ", CredentialUri: credential("search-api-key")}},
		}}},
		{Https: &ateapi.HttpsRule{Hostnames: hosts("*.wikipedia.org", "en.wikipedia.org")}},
		{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: hosts("github.com", "*.githubusercontent.com"), Ports: ports(443)}},
		{Http: &ateapi.HttpRule{Hostnames: hosts("mirror.internal.example"), Ports: ports(80, 8080)}},
	},
	"summarizer": {
		{Https: &ateapi.HttpsRule{Hostnames: hosts("llm.example.com"), Effects: &ateapi.HttpRuleEffects{
			ReplaceHeaders: []*ateapi.CredentialHeader{
				{Header: "x-api-key", CredentialUri: credential("llm-key")},
				{Header: "X-Actor-Token", ActorJwt: &ateapi.ActorJwtSource{Audiences: []string{"https://tools.example.com"}, ExpirationSeconds: 900}},
			},
		}}},
	},
	"triage-bot": {
		{Https: &ateapi.HttpsRule{Hostnames: hosts("*.zendesk.example.com"), Ports: ports(443, 8443)}},
		{Http: &ateapi.HttpRule{Hostnames: hosts("status.example.com"), Ports: ports()}},
	},
	"repo-agent": {
		{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: hosts("github.com"), Ports: ports(443, 22)}},
		{Https: &ateapi.HttpsRule{Hostnames: hosts("proxy.golang.org", "sum.golang.org", "registry.npmjs.org")}},
	},
	"test-runner": {
		{Https: &ateapi.HttpsRule{Hostnames: hosts("registry.npmjs.org")}},
	},
}

func (b *Backend) GetActorEgressPolicy(_ context.Context, ref *ateapi.ObjectRef) (*ateapi.EgressPolicy, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.actor(ref)
	if err != nil {
		return nil, err
	}
	rules := egressRules[a.ActorTemplate.GetName()]
	if a == b.noEgress || rules == nil {
		return nil, status.Errorf(codes.NotFound, "egress policy %s/%s/default not found", ref.GetAtespace(), ref.GetName())
	}
	p := &ateapi.EgressPolicy{
		Metadata: &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: "default",
			CreateTime: a.Metadata.CreateTime, UpdateTime: a.Metadata.CreateTime},
		Rules: rules,
	}
	return proto.Clone(p).(*ateapi.EgressPolicy), nil
}

func binding(role string, members ...string) *ateapi.Binding {
	return &ateapi.Binding{Role: role, Members: members}
}

func (b *Backend) GetGlobalAccessPolicy(context.Context) (*ateapi.AccessPolicy, error) {
	latency()
	return &ateapi.AccessPolicy{
		Metadata: &ateapi.ResourceMetadata{Name: "default", CreateTime: ts(b.start.Add(-72 * time.Hour))},
		Bindings: []*ateapi.Binding{
			binding("owner", "user:platform-admin@example.com"),
			binding("editor", "user:sa-ci-runner"),
			binding("viewer", "user:oncall@example.com", "user:alice@example.com"),
		},
	}, nil
}

func (b *Backend) GetAtespaceAccessPolicy(_ context.Context, atespace string) (*ateapi.AccessPolicy, error) {
	latency()
	bindings := map[string][]*ateapi.Binding{
		"research": {binding("editor", "user:alice@example.com", "user:sa-research-pipeline"), binding("viewer", "user:bob@example.com")},
		"support":  {binding("owner", "user:carol@example.com")},
	}[atespace]
	if bindings == nil {
		return nil, status.Errorf(codes.NotFound, "access policy %s/default not found", atespace)
	}
	return &ateapi.AccessPolicy{
		Metadata: &ateapi.ResourceMetadata{Name: "default", CreateTime: ts(b.start.Add(-48 * time.Hour))},
		Bindings: bindings,
	}, nil
}

// PodEvents returns the pod lifecycle events every worker pod has, plus a
// probe failure on the first worker and a mount failure on a microVM worker.
func (b *Backend) PodEvents(_ context.Context, namespace, pod string) ([]corev1.Event, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := -1
	for i, w := range b.workers {
		if w.WorkerPod == pod && w.WorkerNamespace == namespace {
			idx = i
		}
	}
	if idx < 0 {
		return nil, nil
	}
	w := b.workers[idx]
	ev := func(typ, reason, msg string, ago time.Duration, count int32) corev1.Event {
		t := metav1.NewTime(time.Now().Add(-ago))
		return corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: fmt.Sprintf("%s.%x", pod, rand.Uint32())},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: pod},
			Type:           typ, Reason: reason, Message: msg, Count: count,
			FirstTimestamp: t, LastTimestamp: t, Source: corev1.EventSource{Component: "kubelet", Host: w.NodeName},
		}
	}
	out := []corev1.Event{
		ev(corev1.EventTypeNormal, "Scheduled", "Successfully assigned "+namespace+"/"+pod+" to "+w.NodeName, 30*time.Hour, 1),
		ev(corev1.EventTypeNormal, "Pulled", `Container image "ghcr.io/agent-substrate/ateworker:v0.6.0" already present on machine`, 30*time.Hour, 1),
		ev(corev1.EventTypeNormal, "Created", "Created container: ateworker", 30*time.Hour, 1),
		ev(corev1.EventTypeNormal, "Started", "Started container ateworker", 30*time.Hour, 1),
	}
	switch idx {
	case 0:
		out = append(out, ev(corev1.EventTypeWarning, "Unhealthy",
			`Readiness probe failed: Get "http://10.8.0.14:9090/readyz": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, 4*time.Minute, 3))
	case 4:
		out = append(out, ev(corev1.EventTypeWarning, "FailedMount",
			`MountVolume.SetUp failed for volume "snapshot-cache" : hostPath type check failed: /var/lib/ate/snapshots is not a directory`, 20*time.Minute, 2))
	}
	return out, nil
}

// podLog is what the demo kubelet kept for one pod.
type podLog struct {
	recs []logRec
	seq  int
}

type logRec struct {
	seq  int
	at   time.Time
	line string
}

const maxPodLog = 5000

// emit appends a line in the worker's JSON format to a pod's log. An empty
// container is a lifecycle line. Callers hold b.mu.
func (b *Backend) emit(pod string, a *ateapi.Actor, container, msg string) {
	pl := b.podLogs[pod]
	if pl == nil {
		pl = &podLog{}
		b.podLogs[pod] = pl
	}
	c := ""
	if container != "" {
		c = fmt.Sprintf(`,"ate.actor.container.name":%q`, container)
	}
	now := time.Now()
	pl.seq++
	pl.recs = append(pl.recs, logRec{seq: pl.seq, at: now, line: fmt.Sprintf(
		`{"time":%q,"message":%q,"labels":{"ate.atespace":%q,"ate.actor.name":%q%s}}`,
		now.Format(time.RFC3339Nano), msg, a.Metadata.Atespace, a.Metadata.Name, c)})
	if over := len(pl.recs) - maxPodLog; over > 0 {
		pl.recs = append(pl.recs[:0], pl.recs[over:]...)
	}
}

// PodLogs serves a pod's kept lines like the kubelet: SinceTime at second
// precision, TailLines, and the RFC 3339 timestamp prefix. While followed,
// the actors on the pod keep writing lines, as a multiplexed worker would.
func (b *Backend) PodLogs(ctx context.Context, _, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	b.mu.Lock()
	recs, last := b.since(pod, 0, opts)
	b.mu.Unlock()
	pr, pw := io.Pipe()
	write := func(rs []logRec) bool {
		for _, r := range rs {
			l := r.line
			if opts.Timestamps {
				l = r.at.UTC().Format(time.RFC3339Nano) + " " + l
			}
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return false
			}
		}
		return true
	}
	go func() {
		defer pw.Close()
		if !write(recs) || !opts.Follow {
			return
		}
		msgs := []string{"tool call: web.search", "tool call: fs.read README.md", "model response 812 tokens", "checkpoint idle timer reset", "GET / 200 4ms"}
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(150+rand.Intn(450)) * time.Millisecond):
			}
			b.mu.Lock()
			var on []*ateapi.Actor
			for _, a := range b.actors {
				if a.Status.GetWorkerAssignment().GetWorkerPod() == pod && a.Status.State == ateapi.ActorState_ACTOR_STATE_RUNNING {
					on = append(on, a)
				}
			}
			if len(on) > 0 {
				b.emit(pod, on[rand.Intn(len(on))], "agent", msgs[i%len(msgs)])
			}
			recs, last = b.since(pod, last, nil)
			b.mu.Unlock()
			if !write(recs) {
				return
			}
		}
	}()
	return pr, nil
}

// since returns a pod's lines after seq that opts selects, and the last seq.
func (b *Backend) since(pod string, after int, opts *corev1.PodLogOptions) ([]logRec, int) {
	pl := b.podLogs[pod]
	if pl == nil {
		return nil, after
	}
	var out []logRec
	for _, r := range pl.recs {
		if r.seq <= after {
			continue
		}
		if opts != nil && opts.SinceTime != nil && r.at.Before(opts.SinceTime.Truncate(time.Second)) {
			continue
		}
		out = append(out, r)
	}
	if opts != nil && opts.TailLines != nil && int64(len(out)) > *opts.TailLines {
		out = out[int64(len(out))-*opts.TailLines:]
	}
	return out, pl.seq
}
