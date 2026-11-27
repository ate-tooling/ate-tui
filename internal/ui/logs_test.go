package ui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ate-tooling/ate-tui/internal/demo"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

// logBackend serves fixed, kubelet-timestamped lines per pod. A pod in
// open keeps its stream open until the stream is cancelled.
type logBackend struct {
	*demo.Backend
	mu    sync.Mutex
	lines map[string][]string
	open  map[string]bool
	calls []logCall
}

type logCall struct {
	pod  string
	opts *corev1.PodLogOptions
	ctx  context.Context
}

func (b *logBackend) PodLogs(ctx context.Context, _, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, logCall{pod, opts, ctx})
	content := strings.Join(b.lines[pod], "\n") + "\n"
	if !b.open[pod] || !opts.Follow {
		return io.NopCloser(strings.NewReader(content)), nil
	}
	pr, pw := io.Pipe()
	go func() {
		io.WriteString(pw, content)
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	return pr, nil
}

func (b *logBackend) call(i int) logCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[i]
}

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// kubeLine is a worker JSON line for actor research/a1 as the kubelet
// returns it with Timestamps set.
func kubeLine(at time.Duration, container, msg string) string {
	c := ""
	if container != "" {
		c = fmt.Sprintf(`,"ate.actor.container.name":%q`, container)
	}
	return t0.Add(at).Format(time.RFC3339Nano) + " " +
		fmt.Sprintf(`{"message":%q,"labels":{"ate.atespace":"research","ate.actor.name":"a1"%s}}`, msg, c)
}

func logActor(state ateapi.ActorState, pod string) *ateapi.Actor {
	a := &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: "research", Name: "a1"},
		Status:   &ateapi.ActorStatus{State: state},
	}
	if pod != "" {
		a.Status.WorkerAssignment = &ateapi.WorkerAssignment{WorkerNamespace: "ate-workers", WorkerPod: pod}
	}
	return a
}

func logModel(t *testing.T, b *logBackend) *Model {
	t.Helper()
	m := New(Options{
		Connect: func(context.Context, string) (Backend, ConnInfo, error) { return b, ConnInfo{Context: "test"}, nil },
		Refresh: time.Hour,
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	msg := m.connect()()
	m.Update(msg)
	return m
}

// run waits for one command's message, running a held wait too.
func run(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if h, ok := msg.(heldMsg); ok {
			return run(t, h.cmd)
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("command did not return")
		return nil
	}
}

// pump feeds log chunks back until until reports true.
func pump(t *testing.T, m *Model, cmd tea.Cmd, until func() bool) tea.Cmd {
	t.Helper()
	for !until() {
		if cmd == nil {
			t.Fatalf("no command left; entries %v", msgs(m))
		}
		_, cmd = m.Update(run(t, cmd))
	}
	return cmd
}

func msgs(m *Model) []string {
	var out []string
	for _, e := range m.logs.entries {
		out = append(out, e.msg)
	}
	return out
}

func TestLogsFollowMigration(t *testing.T) {
	b := &logBackend{Backend: demo.New(),
		lines: map[string][]string{
			"pod-a": {kubeLine(0, "", "restored"), kubeLine(time.Second, "agent", "a2")},
			// The first line is from an earlier stay on pod-b: not shown.
			"pod-b": {kubeLine(-time.Hour, "agent", "earlier stay"), kubeLine(5*time.Second, "", "restored"), kubeLine(6*time.Second, "agent", "b2")},
		},
		open: map[string]bool{"pod-a": true, "pod-b": true},
	}
	m := logModel(t, b)
	running := ateapi.ActorState_ACTOR_STATE_RUNNING
	cmd := m.openLogs(logActor(running, "pod-a"))
	cmd = pump(t, m, cmd, func() bool { return len(m.logs.entries) == 2 })
	if c := b.call(0); !c.opts.Timestamps || !c.opts.Follow || c.opts.SinceTime != nil || c.opts.TailLines == nil {
		t.Fatalf("first connect options %+v", c.opts)
	}

	// The actor moves: the old stream is cancelled, and once what it read
	// is in, the new pod is streamed.
	if _, c := m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(running, "pod-b")}); c != nil {
		t.Fatal("new stream opened before the old one ended")
	}
	if b.call(0).ctx.Err() == nil {
		t.Fatal("stream on pod-a still open after the move")
	}
	cmd = pump(t, m, cmd, func() bool { return len(m.logs.entries) == 5 })
	want := []string{"restored", "a2", "── moved to ate-workers/pod-b ──", "restored", "b2"}
	if got := msgs(m); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries %q, want %q", got, want)
	}
	// pod-a's cursor is another node's clock: pod-b is read from a minute
	// before it, not from it, and not its whole tail.
	// Once the follow stream ended, pod-a was read once more up to now, for
	// the lines the cancel cut off.
	if c := b.call(1); c.pod != "pod-a" || c.opts.Follow || c.opts.SinceTime == nil {
		t.Fatalf("catch-up read %s %+v", c.pod, c.opts)
	}
	if c := b.call(2); c.pod != "pod-b" || c.opts.TailLines != nil || c.opts.SinceTime == nil || !c.opts.SinceTime.Time.Equal(t0.Add(time.Second-clockSkew)) {
		t.Fatalf("second connect %s %+v", c.pod, c.opts)
	}
	if !strings.Contains(m.View(), "pod-b") {
		t.Error("title does not show the new pod")
	}

	// Not running: one wait line per state, and the stream closes.
	for _, st := range []ateapi.ActorState{ateapi.ActorState_ACTOR_STATE_SUSPENDING, ateapi.ActorState_ACTOR_STATE_SUSPENDED,
		ateapi.ActorState_ACTOR_STATE_SUSPENDED, ateapi.ActorState_ACTOR_STATE_RESUMING} {
		m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(st, "")})
	}
	if b.call(2).ctx.Err() == nil {
		t.Fatal("stream still open while the actor is not running")
	}
	pump(t, m, cmd, func() bool { return m.logs.stream == 0 })
	if m.logs.stream != 0 {
		t.Fatal("stream still open while the actor is not running")
	}
	got := msgs(m)[5:]
	want = []string{"waiting: actor is Suspending", "waiting: actor is Suspended", "waiting: actor is Resuming"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wait lines %q, want %q", got, want)
	}
	m.closePager()
}

func TestLogsKeptAfterDelete(t *testing.T) {
	b := &logBackend{Backend: demo.New(), lines: map[string][]string{
		"pod-a": {kubeLine(0, "agent", "working"), kubeLine(time.Second, "", "torn down")},
	}, open: map[string]bool{"pod-a": true}}
	m := logModel(t, b)
	cmd := m.openLogs(logActor(ateapi.ActorState_ACTOR_STATE_RUNNING, "pod-a"))
	cmd = pump(t, m, cmd, func() bool { return len(m.logs.entries) == 2 })
	// Deleting stops the stream; then the actor is gone.
	m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(ateapi.ActorState_ACTOR_STATE_DELETING, "")})
	b.mu.Lock()
	b.lines["pod-a"] = append(b.lines["pod-a"], kubeLine(2*time.Second, "", "last words"))
	b.open["pod-a"] = false
	b.mu.Unlock()
	m.Update(logActorMsg{gen: m.logs.gen, err: status.Error(codes.NotFound, "actor not found")})
	// The cancelled stream ends, then what pod-a kept is read once.
	for cmd != nil {
		_, cmd = m.Update(run(t, cmd))
	}
	if !m.logs.kept || m.pager == nil {
		t.Fatal("pager not kept after the delete")
	}
	want := []string{"working", "torn down", "waiting: actor is Deleting",
		"── actor deleted; showing what ate-workers/pod-a kept ──", "last words"}
	if got := msgs(m); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("entries %q, want %q", got, want)
	}
	if c := b.call(len(b.calls) - 1); c.opts.Follow || c.opts.SinceTime == nil {
		t.Fatalf("final read options %+v", c.opts)
	}
	if m.pollLogActor() != nil {
		t.Error("still polling a deleted actor")
	}
	if !strings.Contains(m.View(), "kept by ate-workers/pod-a") {
		t.Error("title does not say where the lines are kept")
	}
}

func TestLogsSamePodReconnectHasNoDuplicates(t *testing.T) {
	b := &logBackend{Backend: demo.New(), lines: map[string][]string{
		"pod-a": {kubeLine(100*time.Millisecond, "agent", "one"), kubeLine(1200*time.Millisecond, "agent", "two"),
			kubeLine(1500*time.Millisecond, "agent", "three")},
	}}
	m := logModel(t, b)
	running := ateapi.ActorState_ACTOR_STATE_RUNNING
	pump(t, m, m.openLogs(logActor(running, "pod-a")), func() bool { return m.logs.stream == 0 })

	// SinceTime has second precision: the kubelet re-sends the cursor's
	// second along with the new line.
	b.lines["pod-a"] = []string{kubeLine(1200*time.Millisecond, "agent", "two"),
		kubeLine(1500*time.Millisecond, "agent", "three"), kubeLine(2500*time.Millisecond, "agent", "four")}
	_, cmd := m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(running, "pod-a")})
	pump(t, m, cmd, func() bool { return m.logs.stream == 0 })

	if got := strings.Join(msgs(m), "|"); got != "one|two|three|four" {
		t.Fatalf("entries %q", got)
	}
	c := b.call(1)
	if c.opts.SinceTime == nil || !c.opts.SinceTime.Time.Equal(t0.Add(1500*time.Millisecond)) || c.opts.TailLines != nil {
		t.Fatalf("reconnect options %+v", c.opts)
	}
}

// fillLogs opens a logs pager and appends container, lifecycle and notice
// lines without a stream.
func fillLogs(t *testing.T, width int) *Model {
	t.Helper()
	m := demoModel(t, width, 30)
	m.openLogs(logActor(ateapi.ActorState_ACTOR_STATE_SUSPENDED, ""))
	var lines []logLine
	for i := 0; i < 30; i++ {
		raw := kubeLine(time.Duration(i)*time.Second, "agent", fmt.Sprintf("tool call %d", i))
		if i%3 == 0 {
			raw = kubeLine(time.Duration(i)*time.Second, "", fmt.Sprintf("restore call %d", i))
		}
		_, rest, _ := splitKubeletTimestamp([]byte(raw))
		l, ok := formatLogLine(rest, "research", "a1")
		if !ok {
			t.Fatal("line not kept")
		}
		lines = append(lines, l)
	}
	m.appendLogs(lines)
	return m
}

func TestLogsSourceFilter(t *testing.T) {
	m := fillLogs(t, 120)
	// 30 lines: 10 lifecycle, 20 container, plus the wait notice.
	tests := []struct {
		source string
		rows   int
	}{{"containers", 21}, {"lifecycle", 11}, {"all", 31}}
	for _, tt := range tests {
		press(m, "c")
		if logSourceNames[m.logs.source] != tt.source || len(m.pager.lines) != tt.rows {
			t.Errorf("%s: source %s, %d rows", tt.source, logSourceNames[m.logs.source], len(m.pager.lines))
		}
		if !strings.Contains(m.View(), "["+tt.source+"]") {
			t.Errorf("%s: title does not show the source", tt.source)
		}
	}
}

func TestLogsSearch(t *testing.T) {
	m := fillLogs(t, 120)
	press(m, "/", "call 2", "enter")
	s := m.logs
	// "call 2" and "call 20" to "call 29".
	if len(s.matches) != 11 {
		t.Fatalf("%d matches", len(s.matches))
	}
	first := s.match
	if !strings.Contains(m.pager.lines[s.matches[first]], "call 2") {
		t.Fatalf("match row %q", m.pager.lines[s.matches[first]])
	}
	press(m, "n")
	if s.match != (first+1)%11 || m.pager.follow {
		t.Fatalf("n: match %d, follow %v", s.match, m.pager.follow)
	}
	press(m, "N", "N")
	if s.match != (first+10)%11 {
		t.Fatalf("N: match %d", s.match)
	}
	// Matches follow the source filter: 21, 24 and 27 are lifecycle.
	press(m, "c", "c")
	if len(s.matches) != 3 {
		t.Fatalf("lifecycle matches %d, want 3", len(s.matches))
	}
	press(m, "esc")
	if s.query != "" || len(s.matches) != 0 || m.pager == nil {
		t.Fatalf("esc: query %q, matches %d", s.query, len(s.matches))
	}
	press(m, "esc")
	if m.pager != nil {
		t.Fatal("second esc did not close the pager")
	}
}

func TestLogHighlight(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0) // termenv.TrueColor, so styles render
	defer lipgloss.SetColorProfile(old)
	l := newLogLine(lineContainer, "", "Tool call TOOL", false)
	want := stMatch.Render("Tool") + " call " + stMatch.Render("TOOL")
	if got := l.render("tool"); got != want {
		t.Fatalf("render %q, want %q", got, want)
	}
	if got := l.render("absent"); got != l.text {
		t.Fatalf("no match should reuse the cached text, got %q", got)
	}
}

func TestLogsWrapAndCap(t *testing.T) {
	m := fillLogs(t, 60)
	long := newLogLine(lineContainer, "", strings.Repeat("x", 150), false)
	m.appendLogs([]logLine{long})
	rows := len(m.pager.lines)
	press(m, "w")
	if len(m.pager.lines) != rows+2 || len(m.logs.rowSeq) != len(m.pager.lines) {
		t.Fatalf("wrap: %d rows, want %d", len(m.pager.lines), rows+2)
	}
	press(m, "w")

	var many []logLine
	for i := 0; i < maxLogLines+50; i++ {
		many = append(many, newLogLine(lineContainer, "", fmt.Sprint(i), false))
	}
	m.appendLogs(many)
	s := m.logs
	if len(s.entries) != maxLogLines || len(s.rowSeq) != len(m.pager.lines) || len(m.pager.lines) != maxLogLines {
		t.Fatalf("cap: %d entries, %d rows", len(s.entries), len(m.pager.lines))
	}
	if s.entries[len(s.entries)-1].msg != fmt.Sprint(maxLogLines+49) {
		t.Fatal("cap dropped the newest lines")
	}
}

// BenchmarkLogRebuild is the cost of a search, filter or wrap change on a
// full 20k-line logs pager.
func BenchmarkLogRebuild(b *testing.B) {
	m := New(Options{Refresh: time.Hour})
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	m.pager = &pager{follow: true}
	m.logs = &logStream{}
	lines := make([]logLine, maxLogLines)
	for i := range lines {
		raw := kubeLine(time.Duration(i)*time.Millisecond, "agent", fmt.Sprintf("tool call %d: GET /v1/search?q=item-%d 200 4ms", i, i))
		_, rest, _ := splitKubeletTimestamp([]byte(raw))
		lines[i], _ = formatLogLine(rest, "research", "a1")
	}
	m.appendLogs(lines)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.logs.query = []string{"", "item-12"}[i%2]
		m.rebuildLogs()
	}
}

func TestFormatLogLineContainerJSON(t *testing.T) {
	const labels = `"labels":{"ate.actor.container.name":"harness","ate.actor.name":"a1","ate.atespace":"research"%s}`
	line := func(fields, extraLabels string) []byte {
		return []byte("{" + fmt.Sprintf(labels, extraLabels) + `,"time":"2026-11-26T21:37:23.148Z"` + fields + "}")
	}
	tests := []struct {
		name, fields, labels, want string
	}{
		{"plain text", `,"message":"hello"`, "", "hello"},
		{"json without message", `,"output":{"text":"Red"},"stopReason":"end_turn"`, "", `{"output":{"text":"Red"},"stopReason":"end_turn"}`},
		{"json with message", `,"msg":"call failed","level":"error","error":"a<b"`, "", `call failed {"error":"a<b","level":"error"}`},
		{"own labels", `,"message":"hi"`, `,"team":"x"`, `hi {"labels":{"team":"x"}}`},
		{"big number", `,"id":12345678901234567890`, "", `{"id":12345678901234567890}`},
		{"non-string message", `,"message":{"a":1}`, "", `{"message":{"a":1}}`},
	}
	for _, tt := range tests {
		l, ok := formatLogLine(line(tt.fields, tt.labels), "research", "a1")
		if !ok {
			t.Fatalf("%s: line not kept", tt.name)
		}
		if l.msg != tt.want {
			t.Errorf("%s: msg %q, want %q", tt.name, l.msg, tt.want)
		}
	}
}

func TestFormatLogLineLifecycleHidesTrace(t *testing.T) {
	b := []byte(`{"time":"2026-11-26T21:37:23Z","message":"Actor resumed","trace_id":"abc","labels":{"ate.actor.name":"a1","ate.atespace":"research"}}`)
	l, ok := formatLogLine(b, "research", "a1")
	if !ok || l.msg != "Actor resumed" {
		t.Fatalf("msg %q ok %v", l.msg, ok)
	}
}
