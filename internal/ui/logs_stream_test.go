package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ate-tooling/ate-tui/internal/demo"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// A line past maxLineBytes is shown truncated and the stream goes on: a
// scanner would end the stream on it, and every reconnect from the cursor
// would hit it again.
func TestLogsOverlongLineDoesNotEndStream(t *testing.T) {
	huge := kubeLine(time.Second, "agent", strings.Repeat("x", 2*maxLineBytes))
	b := &logBackend{Backend: demo.New(), lines: map[string][]string{
		"pod-a": {kubeLine(0, "agent", "before"), huge, kubeLine(2*time.Second, "agent", "after")},
	}}
	m := logModel(t, b)
	pump(t, m, m.openLogs(logActor(ateapi.ActorState_ACTOR_STATE_RUNNING, "pod-a")),
		func() bool { return m.logs.stream == 0 })
	got := msgs(m)
	if len(got) != 3 || got[0] != "before" || got[2] != "after" {
		t.Fatalf("entries %d: %.80q", len(got), got)
	}
	cut := m.logs.entries[1]
	if cut.kind != lineContainer || !strings.HasPrefix(cut.msg, "xxxx") ||
		!strings.HasSuffix(cut.msg, fmt.Sprintf("(truncated: the line is %d bytes)", len(huge))) {
		t.Fatalf("truncated entry kind %v msg …%q", cut.kind, cut.msg[max(len(cut.msg)-60, 0):])
	}
	if !cut.stamp.Equal(t0.Add(time.Second)) {
		t.Fatalf("truncated entry stamp %v", cut.stamp)
	}
}

func TestTruncatedLogLineMatchesLabels(t *testing.T) {
	head := []byte(`{"message":"abcé\`)
	tail := []byte(`zz","labels":{"ate.atespace":"research","ate.actor.name":"a1"}}`)
	l, ok := truncatedLogLine(head, tail, 5000, t0, "research", "a1")
	if !ok || l.kind != lineLifecycle || !strings.HasPrefix(l.msg, "abcé …") {
		t.Fatalf("ok %v kind %v msg %q", ok, l.kind, l.msg)
	}
	if _, ok := truncatedLogLine(head, tail, 5000, t0, "research", "a2"); ok {
		t.Fatal("another actor's line kept")
	}
	sb := []byte(`","labels":{"ate.atespace":"research","ate.actor.name":"a1","ate.actor.container.name":"_sandbox"}}`)
	if _, ok := truncatedLogLine(head, sb, 5000, t0, "research", "a1"); ok {
		t.Fatal("sandbox line kept")
	}
}

// Lines the old pod's stream has read but the pager has not taken yet are
// shown before the move, not dropped with the stream.
func TestLogsMoveKeepsBufferedLines(t *testing.T) {
	b := &logBackend{Backend: demo.New(),
		lines: map[string][]string{
			"pod-a": {kubeLine(0, "agent", "a1"), kubeLine(time.Second, "agent", "crash")},
			"pod-b": {kubeLine(5*time.Second, "agent", "b1")},
		},
		open: map[string]bool{"pod-a": true, "pod-b": true},
	}
	m := logModel(t, b)
	running := ateapi.ActorState_ACTOR_STATE_RUNNING
	cmd := m.openLogs(logActor(running, "pod-a"))
	// Both lines sit in the channel, unread by the pager.
	deadline := time.Now().Add(5 * time.Second)
	for len(m.logs.ch) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("lines never buffered")
		}
		time.Sleep(time.Millisecond)
	}
	m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(running, "pod-b")})
	pump(t, m, cmd, func() bool { return len(m.logs.entries) == 4 })
	want := "a1|crash|── moved to ate-workers/pod-b ──|b1"
	if got := strings.Join(msgs(m), "|"); got != want {
		t.Fatalf("entries %q, want %q", got, want)
	}
	m.closePager()
}

// Back on a pod it left, the actor resumes from that pod's own cursor.
func TestLogsMoveBackUsesThatPodsCursor(t *testing.T) {
	b := &logBackend{Backend: demo.New(), lines: map[string][]string{
		"pod-a": {kubeLine(0, "agent", "a1")},
		"pod-b": {kubeLine(time.Hour, "agent", "b1")},
	}}
	m := logModel(t, b)
	running := ateapi.ActorState_ACTOR_STATE_RUNNING
	done := func() bool { return m.logs.stream == 0 }
	pump(t, m, m.openLogs(logActor(running, "pod-a")), done)
	_, cmd := m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(running, "pod-b")})
	pump(t, m, cmd, done)
	b.mu.Lock()
	b.lines["pod-a"] = append(b.lines["pod-a"], kubeLine(2*time.Hour, "agent", "a2"))
	b.mu.Unlock()
	_, cmd = m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(running, "pod-a")})
	pump(t, m, cmd, done)
	c := b.call(2)
	if c.pod != "pod-a" || c.opts.SinceTime == nil || !c.opts.SinceTime.Time.Equal(t0) || c.opts.TailLines != nil {
		t.Fatalf("third connect %s %+v", c.pod, c.opts)
	}
	want := "a1|── moved to ate-workers/pod-b ──|b1|── moved to ate-workers/pod-a ──|a2"
	if got := strings.Join(msgs(m), "|"); got != want {
		t.Fatalf("entries %q, want %q", got, want)
	}
}

// Dropping rows with matches in them keeps n/N on the same match.
func TestLogsTrimKeepsMatch(t *testing.T) {
	s := &logStream{rowSeq: []int{1, 2, 3, 4, 5}, matches: []int{0, 1, 3, 4}, match: 2}
	p := &pager{lines: []string{"a", "b", "c", "d", "e"}, offset: 4}
	s.trimRows(p, 3) // drops rows 0 and 1, both matches
	if len(s.matches) != 2 || s.matches[s.match] != 1 {
		t.Fatalf("matches %v, match %d", s.matches, s.match)
	}
	s.match = 0
	s.trimRows(p, 5) // drops the match it is on
	if s.match != 0 || len(s.matches) != 1 {
		t.Fatalf("matches %v, match %d", s.matches, s.match)
	}
}

func TestLogsByteBudgetAndWrapCap(t *testing.T) {
	m := fillLogs(t, 60)
	big := strings.Repeat("y", 1<<20)
	var lines []logLine
	for i := 0; i < 40; i++ {
		lines = append(lines, newLogLine(lineContainer, "", fmt.Sprintf("%d %s", i, big), false))
	}
	m.appendLogs(lines)
	s := m.logs
	if s.bytes > maxLogBytes || len(s.entries) >= 40 {
		t.Fatalf("%d entries, %d bytes", len(s.entries), s.bytes)
	}
	total := 0
	for i := range s.entries {
		total += s.entries[i].size()
	}
	if total != s.bytes || !strings.HasPrefix(s.entries[len(s.entries)-1].msg, "39 ") {
		t.Fatalf("bytes %d, counted %d", total, s.bytes)
	}
	if len(s.rowSeq) != len(m.pager.lines) || len(m.pager.lines) != len(s.entries) {
		t.Fatalf("%d rows for %d entries", len(m.pager.lines), len(s.entries))
	}

	press(m, "w")
	if want := len(s.entries) * maxWrapRows; len(m.pager.lines) != want {
		t.Fatalf("wrapped to %d rows, want %d", len(m.pager.lines), want)
	}
	if last := m.pager.lines[maxWrapRows-1]; !strings.HasSuffix(last, "…") || ansi.StringWidth(last) > m.logWidth() {
		t.Fatalf("last row of a cut entry %q", last)
	}
}

func TestTableAlignsGraphemeWidths(t *testing.T) {
	rows := []row{newRow("1", nil, lvNormal, []string{"❤️ok", "x"}, nil), newRow("2", nil, lvNormal, []string{"abcd", "x"}, nil)}
	var ts tableState
	ts.rebuild(rows, []string{"A", "B"}, "")
	a := renderCells(rows[0].cells, ts.widths, "")
	b := renderCells(rows[1].cells, ts.widths, "")
	if ansi.StringWidth(a) != ansi.StringWidth(b) || strings.Index(a, "x") == -1 {
		t.Fatalf("rows %q (%d) and %q (%d)", a, ansi.StringWidth(a), b, ansi.StringWidth(b))
	}
	// Truncated to one column narrower than the emoji row.
	c := renderCells([]string{"❤️❤️❤️", "x"}, []int{3, 1}, "")
	if w := ansi.StringWidth(c); w != 3+colGap+1 {
		t.Fatalf("truncated row %q width %d", c, w)
	}
}

// A click on the list's bottom border or below is not a row.
func TestMousePagerIgnoresBottomBorder(t *testing.T) {
	m := demoModel(t, 120, 30)
	m.newPager("relations")
	p := m.pager
	p.loading = false
	for i := 0; i < 100; i++ {
		p.items = append(p.items, relation{rel: "worker", kind: "actors", name: fmt.Sprint(i)})
	}
	m.drawRelations()
	h := m.pagerHeight()
	click := func(y int) {
		m.Update(tea.MouseMsg{X: 5, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		m.lastClick = time.Time{}
	}
	click(headerHeight + h) // the last row in the box: item h-2
	if p.cursor != h-2 {
		t.Fatalf("last row selected item %d, want %d", p.cursor, h-2)
	}
	p.cursor = 0
	m.drawRelations()
	for _, y := range []int{headerHeight + h + 1, headerHeight + h + 2, m.height - 1} {
		click(y)
		if p.cursor != 0 {
			t.Fatalf("click at y %d selected item %d", y, p.cursor)
		}
	}
}

func TestPagerScrollToEndResumesFollow(t *testing.T) {
	p := &pager{follow: true, lines: make([]string, 50)}
	p.scroll(len(p.lines), 10)
	p.scroll(-1, 10)
	if p.follow {
		t.Fatal("scrolling up did not pause follow")
	}
	p.scroll(1, 10)
	if !p.follow {
		t.Fatal("scrolling back to the end did not resume follow")
	}
	p.scroll(-5, 10)
	p.scroll(len(p.lines), 10) // G
	if !p.follow {
		t.Fatal("G did not resume follow")
	}
	// f turning follow off is not undone by scrolling.
	p.follow = false
	p.scroll(1, 10)
	if p.follow {
		t.Fatal("scrolling resumed follow that f turned off")
	}
}

// Lines the old pod logged that its follow stream had not delivered when the
// actor moved are read once more before the move notice.
func TestLogsMoveCatchesUpOldPod(t *testing.T) {
	b := &logBackend{Backend: demo.New(),
		lines: map[string][]string{
			"pod-a": {kubeLine(0, "agent", "a1")},
			"pod-b": {kubeLine(time.Hour, "agent", "b1")},
		},
		open: map[string]bool{"pod-a": true, "pod-b": true},
	}
	m := logModel(t, b)
	running := ateapi.ActorState_ACTOR_STATE_RUNNING
	cmd := m.openLogs(logActor(running, "pod-a"))
	cmd = pump(t, m, cmd, func() bool { return len(m.logs.entries) == 1 })
	// The crash line reaches the kubelet but not this follow stream.
	b.mu.Lock()
	b.lines["pod-a"] = append(b.lines["pod-a"], kubeLine(time.Second, "agent", "crash"))
	b.mu.Unlock()
	m.Update(logActorMsg{gen: m.logs.gen, actor: logActor(running, "pod-b")})
	pump(t, m, cmd, func() bool { return len(m.logs.entries) == 4 })
	want := "a1|crash|── moved to ate-workers/pod-b ──|b1"
	if got := strings.Join(msgs(m), "|"); got != want {
		t.Fatalf("entries %q, want %q", got, want)
	}
}
