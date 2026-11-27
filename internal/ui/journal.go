package ui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// journalEntry is one line of journal.jsonl.
type journalEntry struct {
	Time    time.Time `json:"time"`
	Context string    `json:"context"`
	Action  string    `json:"action"`
	Mode    string    `json:"mode,omitempty"`
	Object  string    `json:"object"`
	// Worker and Pod are where a deleted actor ran: the pod keeps its
	// logs, which l on the worker lists.
	Worker string `json:"worker,omitempty"`
	Pod    string `json:"pod,omitempty"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// journalMu serializes appends from parallel action results.
var journalMu sync.Mutex

const (
	// maxJournal is how many entries the journal keeps and shows.
	maxJournal = 5000
	// maxJournalBytes is the size past which an append trims the file to
	// its last maxJournal entries.
	maxJournalBytes = 4 << 20
	// maxJournalLine is the longest line the reader takes; a longer one is
	// skipped, as is a line that does not parse.
	maxJournalLine = 1 << 20
)

func appendJournal(path string, e journalEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	journalMu.Lock()
	defer journalMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Another ate-tui may append or trim at the same time: a trim renames
	// a new file over the journal, and an append to the old one would be
	// lost.
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	fi, err := f.Stat()
	if err := f.Close(); err != nil {
		return err
	}
	if err == nil && fi.Size() > maxJournalBytes {
		return trimJournal(path)
	}
	return nil
}

// trimJournal rewrites the journal to its last maxJournal entries, and to
// at most half of maxJournalBytes so the next appends do not trim again,
// through a temp file renamed over it, so a crash leaves the old or the
// new one. The caller holds journalMu and the lock file.
func trimJournal(path string) error {
	es, err := readJournal(path)
	if err != nil {
		return err
	}
	lines := make([][]byte, 0, len(es))
	size := 0
	for i := len(es) - 1; i >= 0; i-- {
		b, err := json.Marshal(es[i])
		if err != nil {
			return err
		}
		if size+len(b)+1 > maxJournalBytes/2 && len(lines) > 0 {
			break
		}
		size += len(b) + 1
		lines = append(lines, b)
	}
	var buf bytes.Buffer
	for i := len(lines) - 1; i >= 0; i-- {
		buf.Write(append(lines[i], '\n'))
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".journal-*.jsonl")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readJournal returns the last maxJournal entries, skipping lines it
// cannot parse and lines longer than maxJournalLine.
func readJournal(path string) ([]journalEntry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []journalEntry
	r := bufio.NewReaderSize(f, 64*1024)
	var line []byte
	long := false
	for {
		chunk, err := r.ReadSlice('\n')
		switch {
		case err == bufio.ErrBufferFull:
			if !long && len(line)+len(chunk) <= maxJournalLine {
				line = append(line, chunk...)
			} else {
				long, line = true, line[:0]
			}
			continue
		case err != nil && err != io.EOF:
			return tail(out), err
		}
		if !long && len(line)+len(chunk) <= maxJournalLine {
			line = append(line, chunk...)
			var e journalEntry
			if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &e) == nil {
				out = append(out, e)
				if len(out) >= 2*maxJournal {
					out = append(out[:0], out[len(out)-maxJournal:]...)
				}
			}
		}
		line, long = line[:0], false
		if err == io.EOF {
			return tail(out), nil
		}
	}
}

// tail is the last maxJournal entries of es.
func tail(es []journalEntry) []journalEntry {
	if over := len(es) - maxJournal; over > 0 {
		return es[over:]
	}
	return es
}

// journal records an action result.
func (m *Model) journal(bt *actionBatch, id string, target any, err error) tea.Cmd {
	e := journalEntry{Time: time.Now(), Context: m.info.Context, Action: bt.act.name, Object: id, Result: "ok"}
	if a, ok := target.(*ateapi.Actor); ok && bt.act.name == "delete" {
		if wa := a.GetStatus().GetWorkerAssignment(); wa.GetWorkerPod() != "" {
			e.Worker, e.Pod = wa.GetWorker().GetName(), wa.GetWorkerNamespace()+"/"+wa.GetWorkerPod()
		}
	}
	if bt.act.name == "suspend" {
		e.Mode = "async"
		if bt.in.sync {
			e.Mode = "sync"
		}
	}
	if bt.act.prompt != "" {
		e.Mode = fmt.Sprintf("%s=%d", bt.act.prompt, bt.in.number)
	}
	if err != nil {
		e.Result, e.Error = "error", errString(err)
	}
	return m.record(e)
}

// record keeps e in memory now and appends it to disk off the UI goroutine.
func (m *Model) record(e journalEntry) tea.Cmd {
	m.session = append(m.session, e)
	if over := len(m.session) - maxJournal; over > 0 {
		m.session = append(m.session[:0], m.session[over:]...)
	}
	path := m.opts.JournalPath
	if path == "" {
		return nil
	}
	return func() tea.Msg {
		if err := appendJournal(path, e); err != nil {
			return journalMsg{err: err}
		}
		return nil
	}
}

type journalLoadedMsg struct {
	gen     int
	entries []journalEntry
	err     error
}

// openJournal lists this session's actions after the ones from earlier
// sessions, which are read from disk once.
func (m *Model) openJournal() tea.Cmd {
	p := m.newPager(stTitleKind.Render("journal"))
	p.follow = true
	gen := p.gen
	p.reload = func() tea.Cmd {
		if !m.journalLoaded {
			return nil
		}
		lines := m.journalLines()
		return func() tea.Msg { return pagerMsg{gen: gen, lines: lines} }
	}
	if m.journalLoaded || m.opts.JournalPath == "" {
		m.journalLoaded = true
		p.loading = false
		p.setLines(m.journalLines(), m.pagerHeight())
		return nil
	}
	path, started := m.opts.JournalPath, m.started
	return func() tea.Msg {
		es, err := readJournal(path)
		// This session's entries are already in memory.
		kept := es[:0]
		for _, e := range es {
			if e.Time.Before(started) {
				kept = append(kept, e)
			}
		}
		return journalLoadedMsg{gen: gen, entries: kept, err: err}
	}
}

func (m *Model) journalLines() []string {
	var out []string
	if m.journalErr != nil {
		out = append(out, stStatusErr.Render("journal: "+m.journalErr.Error()), "")
	}
	render := func(e journalEntry) string {
		res := fg(cGreen).Render("ok")
		if e.Result != "ok" {
			res = stStatusErr.Render("error: " + e.Error)
		}
		act := e.Action
		if e.Mode != "" {
			act += " (" + e.Mode + ")"
		}
		where := ""
		if e.Pod != "" {
			where = stDim.Render(" on "+dash(e.Worker)+" ("+e.Pod+")") + " "
		}
		return stLogTime.Render(e.Time.Local().Format("2006-01-02 15:04:05")) + "  " +
			stContext.Render(e.Context) + "  " + stResource.Render(act) + "  " +
			fg(cBlue).Render(e.Object) + " " + where + " " + res
	}
	if len(m.prior) > 0 {
		out = append(out, stSection.Render("Earlier"))
		for _, e := range m.prior {
			out = append(out, render(e))
		}
		out = append(out, "")
	}
	out = append(out, stSection.Render("This session"))
	if len(m.session) == 0 {
		out = append(out, stDim.Render("no actions yet"))
	}
	for _, e := range m.session {
		out = append(out, render(e))
	}
	if m.opts.JournalPath != "" {
		out = append(out, "", stDim.Render("written to "+m.opts.JournalPath))
	}
	return out
}
