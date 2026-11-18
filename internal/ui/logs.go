package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The logs pager follows an actor the way `kubectl ate logs -f` does
// (runFollow in kubectl-ate): each poll reads the actor, a move to another
// worker pod closes the stream and opens one there, and a reconnect to the
// same pod resumes from the kubelet timestamp of the last line shown. The
// streams run in goroutines; Update only decides which one should be open.

const (
	maxLogLines = 20000
	// logTail bounds the history read on the first connect.
	logTail = 5000
)

// logSource is the `c` filter. The names are kubectl ate logs --source's.
type logSource int

const (
	srcAll logSource = iota
	srcContainers
	srcLifecycle
)

var logSourceNames = [...]string{"all", "containers", "lifecycle"}

type lineKind int

const (
	lineContainer lineKind = iota
	lineLifecycle
	// lineNotice is ate-tui's own: a move, a wait, an error. Every filter
	// shows them.
	lineNotice
)

// logLine is one entry of the logs pager.
type logLine struct {
	seq   int
	stamp time.Time // the kubelet's, the resume cursor
	kind  lineKind
	isErr bool
	head  string // styled time and source
	msg   string
	lower string
	text  string // head + styled msg, the render without a search
}

func newLogLine(kind lineKind, head, msg string, isErr bool) logLine {
	l := logLine{kind: kind, head: head, msg: msg, lower: strings.ToLower(msg), isErr: isErr}
	l.text = head + l.style()(msg)
	return l
}

func notice(msg string) logLine   { return newLogLine(lineNotice, "", msg, false) }
func logError(msg string) logLine { return newLogLine(lineNotice, "", msg, true) }

func (l *logLine) style() func(...string) string {
	switch {
	case l.isErr:
		return stStatusErr.Render
	case l.kind == lineNotice:
		return stDim.Render
	}
	return plain
}

func plain(s ...string) string { return strings.Join(s, " ") }

// render draws the line with every match of q (lowercase) highlighted.
func (l *logLine) render(q string) string {
	if q == "" || !strings.Contains(l.lower, q) {
		return l.text
	}
	st := l.style()
	// ToLower can change byte lengths outside ASCII; highlight the whole
	// message rather than misplace the match.
	if len(l.lower) != len(l.msg) {
		return l.head + stMatch.Render(l.msg)
	}
	var b strings.Builder
	b.WriteString(l.head)
	rest, lower := l.msg, l.lower
	for {
		i := strings.Index(lower, q)
		if i < 0 {
			if rest != "" {
				b.WriteString(st(rest))
			}
			return b.String()
		}
		if i > 0 {
			b.WriteString(st(rest[:i]))
		}
		b.WriteString(stMatch.Render(rest[i : i+len(q)]))
		rest, lower = rest[i+len(q):], lower[i+len(q):]
	}
}

// logStream is the open logs pager's follow loop and its lines.
type logStream struct {
	gen int // the pager's
	ref *ateapi.ObjectRef

	// The stream in flight; stream is 0 when none is.
	stream  int
	ch      chan logLine
	cancel  context.CancelFunc
	ns, pod string

	// lastPod is the pod the cursor belongs to, and cursor the kubelet
	// timestamp of the last line shown from it.
	lastPod   string
	cursor    time.Time
	waiting   bool
	waitState ateapi.ActorState
	polling   bool
	lastErr   string

	entries []logLine
	seq     int

	source    logSource
	wrap      bool
	searching bool
	query     string // lowercase
	// rowSeq is the entry behind each pager row; matches are the rows
	// where a matching entry starts, and match the one n/N is on.
	rowSeq  []int
	matches []int
	match   int
}

func (s *logStream) stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.stream, s.cancel, s.ch = 0, nil, nil
}

func (s *logStream) shows(l *logLine) bool {
	switch s.source {
	case srcContainers:
		return l.kind != lineLifecycle
	case srcLifecycle:
		return l.kind != lineContainer
	}
	return true
}

type (
	logChunkMsg struct {
		gen, stream int
		lines       []logLine
		done        bool
	}
	logActorMsg struct {
		gen   int
		actor *ateapi.Actor
		err   error
	}
)

// waitLines blocks off the UI goroutine for the next lines, then drains
// whatever else is buffered so a burst renders in one frame.
func waitLines(ch <-chan logLine, gen, stream int) tea.Cmd {
	return func() tea.Msg {
		l, ok := <-ch
		if !ok {
			return logChunkMsg{gen: gen, stream: stream, done: true}
		}
		lines := []logLine{l}
		for len(lines) < 2000 {
			select {
			case l, ok := <-ch:
				if !ok {
					return logChunkMsg{gen: gen, stream: stream, lines: lines, done: true}
				}
				lines = append(lines, l)
			default:
				return logChunkMsg{gen: gen, stream: stream, lines: lines}
			}
		}
		return logChunkMsg{gen: gen, stream: stream, lines: lines}
	}
}

func (m *Model) openLogs(a *ateapi.Actor) tea.Cmd {
	md := a.GetMetadata()
	id := md.GetAtespace() + "/" + md.GetName()
	p := m.newPager(stTitleKind.Render("logs") + " " + stTitleCount.Render(id))
	p.follow, p.loading, p.lines = true, false, nil
	m.logs = &logStream{gen: p.gen, ref: &ateapi.ObjectRef{Atespace: md.GetAtespace(), Name: md.GetName()}}
	return m.observeActor(a)
}

// pollLogActor reads the actor for the follow loop, one call at a time.
func (m *Model) pollLogActor() tea.Cmd {
	s := m.logs
	if s == nil || s.polling || m.backend == nil {
		return nil
	}
	s.polling = true
	b, ref, gen := m.backend, s.ref, s.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a, err := b.GetActor(ctx, ref)
		return logActorMsg{gen: gen, actor: a, err: err}
	}
}

func (m *Model) logActor(msg logActorMsg) tea.Cmd {
	s := m.logs
	if s == nil || msg.gen != s.gen {
		return nil
	}
	s.polling = false
	if msg.err != nil {
		if status.Code(msg.err) == codes.NotFound {
			s.stop()
		}
		m.appendLogs([]logLine{logError("get actor: " + errString(msg.err))})
		return nil
	}
	return m.observeActor(msg.actor)
}

// observeActor opens, keeps or closes the stream for what the actor
// reports now.
func (m *Model) observeActor(a *ateapi.Actor) tea.Cmd {
	s := m.logs
	st := a.GetStatus()
	wa := st.GetWorkerAssignment()
	state := st.GetState()
	if wa.GetWorkerPod() == "" || wa.GetWorkerNamespace() == "" || state != ateapi.ActorState_ACTOR_STATE_RUNNING {
		s.stop()
		if !s.waiting || state != s.waitState {
			s.waiting, s.waitState = true, state
			m.appendLogs([]logLine{notice("waiting: actor is " + explain.ActorState(state))})
		}
		return nil
	}
	s.waiting = false
	ns, pod := wa.GetWorkerNamespace(), wa.GetWorkerPod()
	if s.stream != 0 && s.ns == ns && s.pod == pod {
		return nil
	}
	s.stop()
	key := ns + "/" + pod
	switched := key != s.lastPod
	if switched {
		if s.lastPod != "" {
			m.appendLogs([]logLine{notice("── moved to " + key + " ──")})
		}
		s.lastPod = key
	}
	return m.startStream(ns, pod, switched)
}

func (m *Model) startStream(ns, pod string, switched bool) tea.Cmd {
	s := m.logs
	// The kubelet's own timestamp on each line is the cursor: SinceTime is
	// compared against it, and it does not depend on the actor's clock.
	opts := &corev1.PodLogOptions{Follow: true, Timestamps: true}
	if s.cursor.IsZero() {
		tail := int64(logTail)
		opts.TailLines = &tail
	} else {
		opts.SinceTime = &metav1.Time{Time: s.cursor}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.gen++
	s.stream, s.cancel, s.ns, s.pod = m.gen, cancel, ns, pod
	s.ch = make(chan logLine, 4096)
	go streamLogs(ctx, m.backend, ns, pod, opts, s.ref, switched, s.cursor, s.ch)
	return waitLines(s.ch, s.gen, s.stream)
}

// streamLogs reads one pod's lines for one actor until the stream ends or
// ctx is cancelled.
func streamLogs(ctx context.Context, b Backend, ns, pod string, opts *corev1.PodLogOptions,
	ref *ateapi.ObjectRef, switched bool, cursor time.Time, ch chan<- logLine) {
	defer close(ch)
	send := func(l logLine) bool {
		select {
		case ch <- l:
			return true
		case <-ctx.Done():
			return false
		}
	}
	rc, err := b.PodLogs(ctx, ns, pod, opts)
	if err != nil {
		if ctx.Err() == nil {
			send(logError("logs " + ns + "/" + pod + ": " + errString(err)))
		}
		return
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		stamp, line, ok := splitKubeletTimestamp(sc.Bytes())
		// SinceTime has second precision, so a reconnect to the same pod
		// re-reads the cursor's second. Another pod's lines are all new,
		// and its node's clock is not this one's, so they are not compared.
		if ok && !switched && !stamp.After(cursor) {
			continue
		}
		l, ok := formatLogLine(line, ref.GetAtespace(), ref.GetName())
		if !ok {
			continue
		}
		l.stamp = stamp
		if !send(l) {
			return
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		send(logError("logs " + ns + "/" + pod + ": " + err.Error()))
	}
}

// splitKubeletTimestamp splits the RFC 3339 timestamp the kubelet prefixes
// when PodLogOptions.Timestamps is set. ok is false for a line without one,
// which is returned whole.
func splitKubeletTimestamp(line []byte) (time.Time, []byte, bool) {
	i := bytes.IndexByte(line, ' ')
	if i < 0 {
		return time.Time{}, line, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(line[:i]))
	if err != nil {
		return time.Time{}, line, false
	}
	return t, line[i+1:], true
}

func (m *Model) logChunk(msg logChunkMsg) tea.Cmd {
	s := m.logs
	if m.pager == nil || s == nil || msg.gen != s.gen || msg.stream != s.stream {
		return nil
	}
	m.appendLogs(msg.lines)
	if msg.done {
		// The next poll reconnects, from the cursor if the pod is the same.
		s.stop()
		return nil
	}
	return waitLines(s.ch, s.gen, s.stream)
}

// appendLogs adds lines, drops the oldest past maxLogLines, and renders only
// the new ones.
func (m *Model) appendLogs(lines []logLine) {
	s, p := m.logs, m.pager
	if s == nil || p == nil {
		return
	}
	width := m.logWidth()
	for _, l := range lines {
		if l.isErr {
			if l.msg == s.lastErr {
				continue // a failing reconnect repeats itself every poll
			}
			s.lastErr = l.msg
		} else if l.kind != lineNotice {
			s.lastErr = ""
		}
		if !l.stamp.IsZero() {
			s.cursor = l.stamp
		}
		s.seq++
		l.seq = s.seq
		s.entries = append(s.entries, l)
		if s.shows(&l) {
			s.addRows(p, &l, width)
		}
	}
	if over := len(s.entries) - maxLogLines; over > 0 {
		s.entries = append(s.entries[:0], s.entries[over:]...)
		s.trimRows(p, s.entries[0].seq)
	}
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, m.pagerHeight())
}

func (s *logStream) addRows(p *pager, l *logLine, width int) {
	text := l.render(s.query)
	if s.query != "" && strings.Contains(l.lower, s.query) {
		s.matches = append(s.matches, len(p.lines))
	}
	if s.wrap && width > 0 && ansi.StringWidth(text) > width {
		for _, r := range strings.Split(ansi.Wrap(text, width, ""), "\n") {
			p.lines = append(p.lines, r)
			s.rowSeq = append(s.rowSeq, l.seq)
		}
		return
	}
	p.lines = append(p.lines, text)
	s.rowSeq = append(s.rowSeq, l.seq)
}

// trimRows drops the rows of entries older than first.
func (s *logStream) trimRows(p *pager, first int) {
	n := 0
	for n < len(s.rowSeq) && s.rowSeq[n] < first {
		n++
	}
	if n == 0 {
		return
	}
	p.lines = append(p.lines[:0], p.lines[n:]...)
	s.rowSeq = append(s.rowSeq[:0], s.rowSeq[n:]...)
	p.offset -= n
	kept := s.matches[:0]
	for _, r := range s.matches {
		if r >= n {
			kept = append(kept, r-n)
		}
	}
	s.matches = kept
	s.match = min(s.match, max(len(s.matches)-1, 0))
}

// rebuildLogs renders every entry again, after the filter, search, wrap or
// width changes. The top row stays on the same entry.
func (m *Model) rebuildLogs() {
	s, p := m.logs, m.pager
	if s == nil || p == nil {
		return
	}
	top := -1
	if p.offset < len(s.rowSeq) {
		top = s.rowSeq[p.offset]
	}
	p.lines, s.rowSeq, s.matches = p.lines[:0], s.rowSeq[:0], s.matches[:0]
	width := m.logWidth()
	offset := -1
	for i := range s.entries {
		l := &s.entries[i]
		if !s.shows(l) {
			continue
		}
		if offset < 0 && l.seq >= top {
			offset = len(p.lines)
		}
		s.addRows(p, l, width)
	}
	if !p.follow && offset >= 0 {
		p.offset = offset
	}
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, m.pagerHeight())
}

// logWidth is the text width inside the pager box.
func (m *Model) logWidth() int { return m.width - 4 }

// jumpMatch moves to the next (step 1) or previous (-1) match.
func (m *Model) jumpMatch(step int) {
	s := m.logs
	if len(s.matches) == 0 {
		if s.query != "" {
			m.setStatus("no match for "+s.query, true)
		}
		return
	}
	s.match = (s.match + step + len(s.matches)) % len(s.matches)
	m.showMatch()
}

func (m *Model) showMatch() {
	s, p := m.logs, m.pager
	p.follow = false
	h := m.pagerHeight()
	// Keep a few rows of context above the match.
	p.offset = s.matches[s.match] - min(3, h/4)
	p.scroll(0, h)
}

// keyLogs handles the keys only the logs pager has. It reports whether it
// consumed the key.
func (m *Model) keyLogs(msg tea.KeyMsg) bool {
	s, p := m.logs, m.pager
	if s.searching {
		switch msg.Type {
		case tea.KeyEsc:
			s.searching = false
		case tea.KeyEnter:
			s.searching = false
			s.query = strings.ToLower(strings.TrimSpace(m.input))
			m.rebuildLogs()
			// Start from the first match at or below the top of the screen.
			s.match = len(s.matches) - 1
			for i, r := range s.matches {
				if r >= p.offset {
					s.match = i
					break
				}
			}
			if len(s.matches) > 0 {
				m.showMatch()
			} else if s.query != "" {
				m.setStatus("no match for "+s.query, true)
			}
		default:
			m.editInput(msg)
		}
		return true
	}
	switch msg.String() {
	case "/":
		s.searching, m.input = true, s.query
	case "n":
		m.jumpMatch(1)
	case "N":
		m.jumpMatch(-1)
	case "c":
		s.source = (s.source + 1) % logSource(len(logSourceNames))
		m.rebuildLogs()
	case "w":
		s.wrap = !s.wrap
		m.rebuildLogs()
	case "esc":
		if s.query == "" {
			return false
		}
		s.query = ""
		m.rebuildLogs()
	default:
		return false
	}
	return true
}

// logTitle is the pager title with the stream's pod, filter and search.
func (m *Model) logTitle() string {
	s := m.logs
	t := m.pager.title
	switch {
	case s.stream != 0:
		t += stDim.Render("  " + s.ns + "/" + s.pod)
	case s.waiting:
		t += stDim.Render("  " + strings.ToLower(explain.ActorState(s.waitState)))
	}
	t += " " + stTitleFilter.Render("["+logSourceNames[s.source]+"]")
	if s.wrap {
		t += " " + stTitleFilter.Render("[wrap]")
	}
	switch {
	case s.searching:
		t += " " + stTitleFilter.Render("/"+m.input+"█")
	case s.query != "":
		n := fmt.Sprintf(" %d/%d", min(s.match+1, len(s.matches)), len(s.matches))
		t += " " + stTitleFilter.Render("/"+s.query) + stDim.Render(n)
	}
	return t
}

// Log label keys from go.ate.dev/substrate/internal/ateattr.
const (
	labelAtespace  = "ate.atespace"
	labelActor     = "ate.actor.name"
	labelContainer = "ate.actor.container.name"
	sandboxID      = "_sandbox"
)

// formatLogLine keeps lines the worker attributes to this actor. A worker pod
// can host several actors over time, so lines must match both labels.
func formatLogLine(b []byte, atespace, name string) (logLine, bool) {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return logLine{}, false
	}
	var labels map[string]any
	for _, k := range []string{"logging.googleapis.com/labels", "labels"} {
		if l, ok := m[k].(map[string]any); ok {
			labels = l
			break
		}
	}
	if labels[labelActor] != name || labels[labelAtespace] != atespace {
		return logLine{}, false
	}
	container, _ := labels[labelContainer].(string)
	if container == sandboxID {
		return logLine{}, false
	}
	msg, _ := m["message"].(string)
	if msg == "" {
		msg, _ = m["msg"].(string)
	}
	ts := ""
	for _, k := range []string{"time", "timestamp", "ts"} {
		if s, ok := m[k].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				ts = t.Local().Format("15:04:05")
			}
			break
		}
	}
	kind, src := lineLifecycle, stLogLifecycle.Render("lifecycle")
	if container != "" {
		kind, src = lineContainer, stLogContainer.Render(container)
	}
	return newLogLine(kind, stLogTime.Render(ts)+" "+src+" ", msg, false), true
}
