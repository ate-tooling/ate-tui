package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// clockSkew is how far another node's clock may run behind this one's
// when a move starts the new pod's read from the old pod's cursor.
const clockSkew = time.Minute

const (
	maxLogLines = 20000
	// maxLogBytes bounds the text the entries hold, so a burst of long
	// lines cannot take the memory maxLogLines short ones would not.
	maxLogBytes = 64 << 20
	// maxWrapRows bounds the rows one entry wraps to.
	maxWrapRows = 200
	// logTail bounds the history read on the first connect to a pod.
	logTail = 5000
	// maxLineBytes is how much of one log line is read; the rest of a
	// longer line is skipped, so the stream goes on past it.
	maxLineBytes = 1 << 20
	// lineTailBytes is how much of an overlong line's end is kept: the
	// worker writes its labels there.
	lineTailBytes = 4 << 10
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
	kind  lineKind
	isErr bool
	head  string // styled time and source
	raw   string // the message as logged, for the clipboard
	msg   string // the message as one plain row, for search
	lower string
	text  string // head + styled msg, the render without a search
}

func newLogLine(kind lineKind, head, raw string, isErr bool) logLine {
	msg := printable(raw, toPlain)
	l := logLine{kind: kind, head: head, raw: raw, msg: msg, lower: strings.ToLower(msg), isErr: isErr}
	// A container's own colors stay; the reset keeps them off the rest of
	// the row.
	l.text = head + l.style()(msg)
	if kind == lineContainer {
		if shown := printable(raw, toRow); shown != msg {
			l.text = head + shown + ansi.ResetStyle
		}
	}
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
	ch      chan streamedLine
	cancel  context.CancelFunc
	ns, pod string

	// draining is set once the stream in flight is cancelled: what it
	// already read still comes in, and then settle acts on next, the
	// actor as last seen, and gone. pending are the states the actor
	// passed while it drained, for their wait notices.
	draining bool
	// catchingUp is set while the retired pod's last lines are read once
	// more, without following: cancelling the follow stream loses the
	// lines it had in flight.
	catchingUp bool
	next       *ateapi.Actor
	pending    []ateapi.ActorState
	gone       bool

	// lastPod is the pod the cursor belongs to, and cursor the kubelet
	// timestamp of the last line shown from it. Each node has its own
	// clock, so cursors keeps the cursor of every pod left behind.
	lastPod   string
	cursor    time.Time
	cursors   map[string]time.Time
	waiting   bool
	waitState ateapi.ActorState
	polling   bool
	// kept reads what one pod kept for the actor once, without following
	// it: the actor may be gone, or living elsewhere.
	kept    bool
	lastErr string

	// entries are numbered from 1 in the order they came: entries[i] is
	// number dropped+i+1, as rowSeq refers to them.
	entries []logLine
	dropped int
	bytes   int // the text the entries hold

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
	s.stream, s.cancel, s.ch, s.draining, s.catchingUp = 0, nil, nil, false, false
}

// retire cancels the stream in flight but keeps taking the lines it has
// already read, which are often the old pod's last (a crash, a shutdown).
// When it ends, settle acts on what the actor did meanwhile.
func (s *logStream) retire(a *ateapi.Actor) {
	if s.cancel != nil {
		s.cancel()
	}
	s.cancel, s.draining = nil, true
	s.saw(a)
}

// saw records the actor seen while the stream drains.
func (s *logStream) saw(a *ateapi.Actor) {
	if a == nil {
		return
	}
	s.next = a
	if st := a.GetStatus().GetState(); !runningOnPod(a) && (len(s.pending) == 0 || s.pending[len(s.pending)-1] != st) {
		s.pending = append(s.pending, st)
	}
}

// runningOnPod reports an actor whose worker pod can be streamed.
func runningOnPod(a *ateapi.Actor) bool {
	st := a.GetStatus()
	wa := st.GetWorkerAssignment()
	return wa.GetWorkerPod() != "" && wa.GetWorkerNamespace() != "" && st.GetState() == ateapi.ActorState_ACTOR_STATE_RUNNING
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
		// cursor is the kubelet timestamp of the last line that had one.
		cursor time.Time
		done   bool
	}
	logActorMsg struct {
		gen   int
		actor *ateapi.Actor
		err   error
	}
)

// waitLines blocks off the UI goroutine for the next lines, then drains
// whatever else is buffered so a burst renders in one frame.
func waitLines(ch <-chan streamedLine, gen, stream int) tea.Cmd {
	return func() tea.Msg {
		msg := logChunkMsg{gen: gen, stream: stream}
		add := func(l streamedLine) {
			msg.lines = append(msg.lines, l.line)
			if !l.stamp.IsZero() {
				msg.cursor = l.stamp
			}
		}
		l, ok := <-ch
		if !ok {
			msg.done = true
			return msg
		}
		add(l)
		for len(msg.lines) < 2000 {
			select {
			case l, ok := <-ch:
				if !ok {
					msg.done = true
					return msg
				}
				add(l)
			default:
				return msg
			}
		}
		return msg
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
	if s == nil || s.kept || s.polling || m.backend == nil {
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
			if s.stream != 0 {
				// Show what the stream already read before what was kept.
				if !s.draining {
					s.retire(nil)
				}
				s.gone = true
				return nil
			}
			if s.lastPod != "" {
				return m.keepDeleted()
			}
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
	if s.draining {
		s.saw(a)
		return nil
	}
	wa := a.GetStatus().GetWorkerAssignment()
	if !runningOnPod(a) {
		if s.stream != 0 {
			// The wait notice goes after the lines the stream has read.
			s.retire(a)
			return nil
		}
		m.waitNotice(a.GetStatus().GetState())
		return nil
	}
	s.waiting = false
	ns, pod := wa.GetWorkerNamespace(), wa.GetWorkerPod()
	if s.stream != 0 {
		if s.ns != ns || s.pod != pod {
			s.retire(a)
		}
		return nil
	}
	key := ns + "/" + pod
	if key != s.lastPod {
		if s.lastPod != "" {
			m.appendLogs([]logLine{notice("── moved to " + key + " ──")})
			if s.cursors == nil {
				s.cursors = map[string]time.Time{}
			}
			s.cursors[s.lastPod] = s.cursor
		}
		// Another node's clock: resume from this pod's own cursor, if the
		// actor has been here before. Else start a little before the old
		// pod's cursor: what this pod logged for the actor earlier than
		// that is from an earlier stay, already shown or not this run's.
		prev := s.cursor
		s.lastPod, s.cursor = key, s.cursors[key]
		if s.cursor.IsZero() && !prev.IsZero() {
			s.cursor = prev.Add(-clockSkew)
		}
	}
	return m.startStream(ns, pod)
}

func (m *Model) waitNotice(state ateapi.ActorState) {
	s := m.logs
	if !s.waiting || state != s.waitState {
		s.waiting, s.waitState = true, state
		m.appendLogs([]logLine{notice("waiting: actor is " + explain.ActorState(state))})
	}
}

// settle acts, once a retired stream has ended, on what the actor did
// while it drained.
func (m *Model) settle() tea.Cmd {
	s := m.logs
	next, pending, gone := s.next, s.pending, s.gone
	s.next, s.pending, s.gone = nil, nil, false
	for _, st := range pending {
		m.waitNotice(st)
	}
	if gone {
		if s.lastPod != "" {
			return m.keepDeleted()
		}
		return nil
	}
	if next != nil {
		return m.observeActor(next)
	}
	return nil
}

// keepDeleted turns the pager to what the last pod kept once the actor is
// deleted: one read from the cursor picks up its last lines, and the pager
// stays open on them.
func (m *Model) keepDeleted() tea.Cmd {
	s := m.logs
	s.kept, s.waiting = true, false
	m.appendLogs([]logLine{notice("── actor deleted; showing what " + s.lastPod + " kept ──")})
	ns, pod, _ := strings.Cut(s.lastPod, "/")
	return m.startStream(ns, pod)
}

// startStream opens lastPod's stream from its cursor.
func (m *Model) startStream(ns, pod string) tea.Cmd {
	return m.readStream(ns, pod, !m.logs.kept)
}

// readStream reads ns/pod from the cursor, following it or up to now.
func (m *Model) readStream(ns, pod string, follow bool) tea.Cmd {
	s := m.logs
	// The kubelet's own timestamp on each line is the cursor: SinceTime is
	// compared against it, and it does not depend on the actor's clock.
	opts := &corev1.PodLogOptions{Follow: follow, Timestamps: true}
	if s.cursor.IsZero() {
		tail := int64(logTail)
		opts.TailLines = &tail
	} else {
		opts.SinceTime = &metav1.Time{Time: s.cursor}
	}
	// SinceTime has second precision, so a reconnect re-reads the cursor's
	// second; after drops what was shown.
	return m.runStream(ns, pod, opts, s.cursor, time.Time{})
}

// runStream reads ns/pod's lines for the actor stamped after after and, if
// until is set, not after until.
func (m *Model) runStream(ns, pod string, opts *corev1.PodLogOptions, after, until time.Time) tea.Cmd {
	s := m.logs
	ctx, cancel := context.WithCancel(context.Background())
	m.gen++
	s.stream, s.cancel, s.ns, s.pod = m.gen, cancel, ns, pod
	s.ch = make(chan streamedLine, 4096)
	go streamLogs(ctx, m.backend, ns, pod, opts, s.ref, after, until, s.ch)
	return m.wait(waitLines(s.ch, s.gen, s.stream))
}

// streamedLine is a line as the stream read it: stamp is the kubelet's
// timestamp, zero when it had none, and where a reconnect resumes.
type streamedLine struct {
	line  logLine
	stamp time.Time
}

// streamLogs reads one pod's lines for one actor until the stream ends or
// ctx is cancelled. A line without a kubelet timestamp is always kept.
func streamLogs(ctx context.Context, b Backend, ns, pod string, opts *corev1.PodLogOptions,
	ref *ateapi.ObjectRef, after, until time.Time, ch chan<- streamedLine) {
	defer close(ch)
	send := func(l logLine, stamp time.Time) bool {
		select {
		case ch <- streamedLine{l, stamp}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	rc, err := b.PodLogs(ctx, ns, pod, opts)
	if err != nil {
		if ctx.Err() == nil {
			send(logError("logs "+ns+"/"+pod+": "+errString(err)), time.Time{})
		}
		return
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 64<<10)
	var buf, tail []byte
	for {
		line, t, n, err := readLogLine(br, buf, tail)
		buf, tail = line, t
		if n > 0 {
			stamp, rest, ok := splitKubeletTimestamp(line)
			if !ok || stamp.After(after) && (until.IsZero() || !stamp.After(until)) {
				var l logLine
				if n > len(line) {
					l, ok = truncatedLogLine(rest, tail, n, stamp, ref.GetAtespace(), ref.GetName())
				} else {
					l, ok = formatLogLine(rest, ref.GetAtespace(), ref.GetName())
				}
				if ok && !send(l, stamp) {
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				send(logError("logs "+ns+"/"+pod+": "+err.Error()), time.Time{})
			}
			return
		}
	}
}

// readLogLine reads one line without its line ending into line, up to
// maxLineBytes of it; of a longer line, tail is its last lineTailBytes. n is
// the line's whole length. line and tail reuse the given buffers.
func readLogLine(br *bufio.Reader, line, tail []byte) (_, _ []byte, n int, err error) {
	line, tail = line[:0], tail[:0]
	for {
		var frag []byte
		frag, err = br.ReadSlice('\n')
		if err == nil {
			frag = frag[:len(frag)-1]
			if len(frag) > 0 && frag[len(frag)-1] == '\r' {
				frag = frag[:len(frag)-1]
			}
		}
		n += len(frag)
		if room := maxLineBytes - len(line); room > 0 {
			k := min(room, len(frag))
			line = append(line, frag[:k]...)
			frag = frag[k:]
		}
		if len(frag) > 0 {
			tail = append(tail, frag...)
			if over := len(tail) - lineTailBytes; over > 0 {
				tail = append(tail[:0], tail[over:]...)
			}
		}
		if err != bufio.ErrBufferFull {
			return line, tail, n, err
		}
	}
}

// truncatedLogLine keeps an overlong worker line for the actor: head is its
// start after the kubelet timestamp, tail its end, n its length. Neither is
// JSON on its own, so the labels are matched as text and the message shown
// as far as head holds it.
func truncatedLogLine(head, tail []byte, n int, stamp time.Time, atespace, name string) (logLine, bool) {
	label := func(k string) (string, bool) {
		key, _ := json.Marshal(k)
		key = append(key, ':')
		for _, b := range [][]byte{tail, head} {
			if i := bytes.LastIndex(b, key); i >= 0 {
				var v string
				if json.NewDecoder(bytes.NewReader(b[i+len(key):])).Decode(&v) == nil {
					return v, true
				}
			}
		}
		return "", false
	}
	if v, _ := label(labelActor); v != name {
		return logLine{}, false
	}
	if v, _ := label(labelAtespace); v != atespace {
		return logLine{}, false
	}
	container, _ := label(labelContainer)
	if container == sandboxID {
		return logLine{}, false
	}
	container = printable(container, toPlain)
	kind, src := lineLifecycle, stLogLifecycle.Render("lifecycle")
	if container != "" {
		kind, src = lineContainer, stLogContainer.Render(container)
	}
	ts := ""
	if !stamp.IsZero() {
		ts = stamp.Local().Format("15:04:05")
	}
	msg := messagePrefix(head) + fmt.Sprintf(" … (truncated: the line is %d bytes)", n)
	return newLogLine(kind, stLogTime.Render(ts)+" "+src+" ", msg, false), true
}

// messagePrefix is as much of the message or msg string as the start of a
// truncated JSON line holds, at most 64 KiB of it.
func messagePrefix(b []byte) string {
	for _, k := range []string{`"message":"`, `"msg":"`} {
		i := bytes.Index(b, []byte(k))
		if i < 0 {
			continue
		}
		q := b[i+len(k)-1:]
		var v string
		if json.NewDecoder(bytes.NewReader(q)).Decode(&v) == nil {
			return v
		}
		// Cut inside the string: close it, dropping a cut escape.
		q = q[:min(len(q), 64<<10)]
		for cut := 0; cut < 8 && cut < len(q); cut++ {
			if json.Unmarshal(append(q[:len(q)-cut:len(q)-cut], '"'), &v) == nil {
				return v
			}
		}
	}
	return ""
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
	if !msg.cursor.IsZero() {
		s.cursor = msg.cursor
	}
	if msg.done {
		// The next poll reconnects, from the cursor if the pod is the same.
		draining, caughtUp, ns, pod := s.draining, s.catchingUp, s.ns, s.pod
		s.stop()
		switch {
		case draining && !caughtUp && !s.gone:
			// Read what the retired pod logged up to now, then settle. A
			// deleted actor's kept read does the same.
			cmd := m.readStream(ns, pod, false)
			s.draining, s.catchingUp = true, true
			return cmd
		case draining:
			return m.settle()
		}
		return nil
	}
	return m.wait(waitLines(s.ch, s.gen, s.stream))
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
		s.entries = append(s.entries, l)
		s.bytes += l.size()
		if s.shows(&l) {
			s.addRows(p, &l, s.dropped+len(s.entries), width)
		}
	}
	over := 0
	for over < len(s.entries)-1 && (len(s.entries)-over > maxLogLines || s.bytes > maxLogBytes) {
		s.bytes -= s.entries[over].size()
		over++
	}
	if over > 0 {
		n := copy(s.entries, s.entries[over:])
		clear(s.entries[n:])
		s.entries = s.entries[:n]
		s.dropped += over
		s.trimRows(p, s.dropped+1)
	}
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, m.pagerHeight())
}

// size is about the bytes the entry holds.
func (l *logLine) size() int {
	return len(l.head) + len(l.raw) + len(l.msg) + len(l.lower) + len(l.text)
}

// addRows renders l, entry number seq, into p.
func (s *logStream) addRows(p *pager, l *logLine, seq, width int) {
	text := l.render(s.query)
	if s.query != "" && strings.Contains(l.lower, s.query) {
		s.matches = append(s.matches, len(p.lines))
	}
	if s.wrap && width > 0 && ansi.StringWidth(text) > width {
		// A huge line wraps to maxWrapRows; the clipboard has all of it.
		cut := false
		if limit := width * maxWrapRows; ansi.StringWidth(text) > limit {
			text, cut = ansi.Truncate(text, limit, ""), true
		}
		rows := strings.Split(ansi.Wrap(text, width, ""), "\n")
		if len(rows) > maxWrapRows {
			rows, cut = rows[:maxWrapRows], true
		}
		if cut {
			last := len(rows) - 1
			rows[last] = ansi.Truncate(rows[last], width-1, "") + ansi.ResetStyle + "…"
		}
		for _, r := range rows {
			p.lines = append(p.lines, r)
			s.rowSeq = append(s.rowSeq, seq)
		}
		return
	}
	p.lines = append(p.lines, text)
	s.rowSeq = append(s.rowSeq, seq)
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
	// n/N stays on the same match.
	s.match -= len(s.matches) - len(kept)
	s.matches = kept
	s.match = min(max(s.match, 0), max(len(s.matches)-1, 0))
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
		seq := s.dropped + i + 1
		if offset < 0 && seq >= top {
			offset = len(p.lines)
		}
		s.addRows(p, l, seq, width)
	}
	if !p.follow && offset >= 0 {
		p.offset = offset
	}
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, m.pagerHeight())
}

// copyText is the shown entries as logged: the original line breaks and
// tabs, without the pager's wrapping or escape sequences.
func (s *logStream) copyText() string {
	var b strings.Builder
	for i := range s.entries {
		l := &s.entries[i]
		if !s.shows(l) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ansi.Strip(l.head))
		b.WriteString(strings.TrimRight(printable(l.raw, toCopy), "\n"))
	}
	return b.String()
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
	// Like scrolling up: scrolling back to the end resumes follow.
	p.paused = p.paused || p.follow
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
	case s.kept:
		t += stDim.Render("  kept by " + s.ns + "/" + s.pod)
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
	m, labels, ok := parseWorkerLine(b)
	if !ok || labels[labelActor] != name || labels[labelAtespace] != atespace {
		return logLine{}, false
	}
	container, _ := labels[labelContainer].(string)
	container = printable(container, toPlain)
	ts := ""
	for _, k := range []string{"time", "timestamp", "ts"} {
		if s, ok := m[k].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				ts = t.Local().Format("15:04:05")
				delete(m, k)
			}
			break
		}
	}
	kind, src := lineLifecycle, stLogLifecycle.Render("lifecycle")
	msg := ""
	if container != "" {
		kind, src = lineContainer, stLogContainer.Render(container)
		msg = containerMessage(m)
	} else if msg, _ = m["message"].(string); msg == "" {
		msg, _ = m["msg"].(string)
	}
	return newLogLine(kind, stLogTime.Render(ts)+" "+src+" ", msg, false), true
}

// containerMessage renders a container line. The worker wraps plain text as
// message and merges a JSON line's fields into its record, so the fields left
// after the worker's own are the container's: its message or msg, then the
// rest as compact JSON.
func containerMessage(m map[string]any) string {
	for _, k := range labelKeys {
		l, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		for lk := range l {
			if strings.HasPrefix(lk, "ate.") {
				delete(l, lk)
			}
		}
		if len(l) == 0 {
			delete(m, k)
		}
	}
	msg := ""
	for _, k := range []string{"message", "msg"} {
		if s, ok := m[k].(string); ok {
			msg = s
			delete(m, k)
			break
		}
	}
	if len(m) == 0 {
		return msg
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(m) != nil {
		return msg
	}
	rest := strings.TrimSpace(buf.String())
	if msg == "" {
		return rest
	}
	return msg + " " + rest
}

// labelKeys are the spellings of the worker's label group.
var labelKeys = []string{"logging.googleapis.com/labels", "labels"}

// parseWorkerLine decodes a worker JSON line and its labels. ok is false
// for other output and for the sandbox's own lines.
func parseWorkerLine(b []byte) (m, labels map[string]any, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&m) != nil || m == nil {
		return nil, nil, false
	}
	for _, k := range labelKeys {
		if l, ok := m[k].(map[string]any); ok {
			labels = l
			break
		}
	}
	if labels[labelContainer] == sandboxID {
		return nil, nil, false
	}
	return m, labels, true
}
