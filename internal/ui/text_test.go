package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestPrintable(t *testing.T) {
	for _, tt := range []struct {
		name, in, row, copy string
	}{
		{"tab at start", "\tfor i := range 10 {", "        for i := range 10 {", "\tfor i := range 10 {"},
		{"tab mid", "ab\tc", "ab      c", "ab\tc"},
		{"tab after wide", "中\tx", "中      x", "中\tx"},
		{"tab after emoji modifier", "👍🏽\tx", "👍🏽      x", "👍🏽\tx"},
		{"tab after sgr", "\x1b[31mab\x1b[0m\tc", "\x1b[31mab\x1b[0m      c", "ab\tc"},
		{"line breaks", "a\r\nb\nc", "a↵ b↵ c", "a\nb\nc"},
		{"trailing newline", "request done\n", "request done", "request done\n"},
		{"lone cr", "50%\r60%", "50%↵ 60%", "50%\n60%"},
		{"tab after break", "abc\n\tx", "abc↵         x", "abc\n\tx"},
		{"c0 controls", "x\x07y\x00z\x08", "xyz", "xyz"},
		{"cursor moves", "x\x1b[2Jy\x1b[10;1Hz", "xyz", "xyz"},
		{"sgr kept", "\x1b[32mINFO\x1b[0m ok", "\x1b[32mINFO\x1b[0m ok", "INFO ok"},
		{"osc terminated", "a\x1b]0;title\x07b", "ab", "ab"},
		{"osc st", "a\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "alink", "alink"},
		{"osc unterminated", "a\x1b]0;title then text", "a0;title then text", "a0;title then text"},
		{"apc unterminated", "a\x1b_b c", "ab c", "ab c"},
		{"esc newline", "a\x1b\nb", "a↵ b", "a\nb"},
		{"esc at end", "a\x1b", "a", "a"},
		{"two byte escape", "a\x1b7b\x1b(Bc", "abc", "abc"},
		{"invalid utf8 c1", "x\x90y", "x�y", "x�y"},
		{"utf8 c1", "raw\u009b31mred", "raw31mred", "raw31mred"},
		{"bidi override", "abc‮def", "abcdef", "abcdef"},
		{"line separator", "a b", "ab", "ab"},
		{"zwj kept", "👩‍💻", "👩‍💻", "👩‍💻"},
		{"plain", "hello world", "hello world", "hello world"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := printable(tt.in, toRow); got != tt.row {
				t.Errorf("toRow = %q, want %q", got, tt.row)
			}
			if got, want := printable(tt.in, toPlain), ansi.Strip(tt.row); got != want {
				t.Errorf("toPlain = %q, want %q", got, want)
			}
			if got := printable(tt.in, toCopy); got != tt.copy {
				t.Errorf("toCopy = %q, want %q", got, tt.copy)
			}
		})
	}
}

// Every row fit draws is exactly w cells, whatever the text holds.
func TestFitControlCharacters(t *testing.T) {
	for _, s := range []string{
		"\tfor i := range 10 {",
		"multi\nline\nevent",
		"x\x90y z",
		"\x1b[1mbold never reset",
		"a\x1b]0;title",
		stDim.Render("dim\twith tab"),
	} {
		for _, w := range []int{5, 40} {
			got := fit(s, w)
			if strings.ContainsAny(got, "\t\n\r") {
				t.Errorf("fit(%q, %d) = %q keeps a control character", s, w, got)
			}
			if lw := lipgloss.Width(got); lw != w {
				t.Errorf("fit(%q, %d) is %d cells wide", s, w, lw)
			}
		}
	}
	if got := fit("\x1b[1mbold", 10); !strings.HasSuffix(strings.TrimRight(got, " "), ansi.ResetStyle) {
		t.Errorf("fit left a style open: %q", got)
	}
}

func TestLogLineKeepsRawAndColor(t *testing.T) {
	l := newLogLine(lineContainer, "", "\x1b[32mINFO\x1b[0m\tready\n", false)
	if l.raw != "\x1b[32mINFO\x1b[0m\tready\n" {
		t.Errorf("raw = %q", l.raw)
	}
	if l.msg != "INFO    ready" {
		t.Errorf("msg = %q", l.msg)
	}
	if !strings.Contains(l.text, "\x1b[32mINFO") || !strings.HasSuffix(l.text, ansi.ResetStyle) {
		t.Errorf("text = %q, want the container's color and a reset", l.text)
	}
	// A notice is the pager's own styling; its message is plain.
	n := notice("a\x1b[31mb\tc")
	if strings.Contains(n.text, "\x1b[31m") || lipgloss.Width(n.text) != len("ab      c") {
		t.Errorf("notice text = %q", n.text)
	}
	s := &logStream{entries: []logLine{l, n}}
	if got, want := s.copyText(), "INFO\tready\nab\tc"; got != want {
		t.Errorf("copyText = %q, want %q", got, want)
	}
}

// A long row is cut before it is cleaned, with the same result.
func TestFitLongRow(t *testing.T) {
	long := strings.Repeat("a\tb", 1<<18)
	got := fit(long, 40)
	want := fit(strings.Repeat("a\tb", 10), 40)
	if got != want || lipgloss.Width(got) != 40 || !strings.HasSuffix(got, "…") {
		t.Errorf("fit(long) = %q, want %q", got, want)
	}
}
