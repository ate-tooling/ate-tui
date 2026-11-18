package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Output is the program's terminal output. The renderer and OSC 52 copies
// share it under one lock, so a copy never lands inside a frame. It keeps
// *os.File's Fd so Bubble Tea still finds the terminal.
type Output struct {
	mu sync.Mutex
	f  *os.File
}

func NewOutput(f *os.File) *Output { return &Output{f: f} }

func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.f.Write(p)
}

func (o *Output) Read(p []byte) (int, error) { return o.f.Read(p) }
func (o *Output) Close() error               { return nil }
func (o *Output) Fd() uintptr                { return o.f.Fd() }

// maxCopy is about what terminals accept in one OSC 52 sequence.
const maxCopy = 512 << 10

// Copy puts s on the system clipboard with OSC 52, wrapped for tmux or
// screen when running inside one.
func (o *Output) Copy(s string) error {
	seq := osc52.New(s)
	switch {
	case os.Getenv("TMUX") != "":
		seq = seq.Tmux()
	case strings.HasPrefix(os.Getenv("TERM"), "screen"):
		seq = seq.Screen()
	}
	_, err := o.Write([]byte(seq.String()))
	return err
}

type copiedMsg struct {
	what string
	err  error
}

// copy sends text to the clipboard off the UI goroutine.
func (m *Model) copy(text string) tea.Cmd {
	if m.opts.Copy == nil {
		m.setStatus("copy is not available", true)
		return nil
	}
	if len(text) > maxCopy {
		// Keep the end: for logs and journals the newest lines matter.
		text = text[len(text)-maxCopy:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	what := text
	if n := strings.Count(text, "\n"); n > 0 {
		what = strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
		what = ansi.Truncate(what, 40, "…") + " (" + plural(n+1, "line") + ")"
	}
	copyFn := m.opts.Copy
	return func() tea.Msg { return copiedMsg{what: what, err: copyFn(text)} }
}

// pagerText is the pager content without styling.
func pagerText(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(ansi.Strip(l), " ")
	}
	return strings.Join(out, "\n")
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}
