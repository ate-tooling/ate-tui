package ui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Text from the cluster (log lines, event messages, errors, YAML values) can
// hold tabs, newlines and escape sequences. The views count cells with
// ansi/lipgloss widths, which see those as zero cells while the terminal
// moves its cursor on them, so a row drawn as-is spills past its box and
// shifts every row after it. Everything drawn goes through printable first.

// textMode says what printable keeps.
type textMode int

const (
	// toRow makes s one row: tabs expand, line breaks show as ↵, SGR
	// (color) sequences stay and every other sequence goes.
	toRow textMode = iota
	// toPlain is toRow without SGR.
	toPlain
	// toCopy keeps tabs and line breaks (\r\n and a lone \r become \n) and
	// drops every sequence, for the clipboard.
	toCopy
)

const (
	tabStop   = 8
	breakMark = "↵ "
	spaces    = "        "
)

// printable returns s safe to measure and draw (see textMode). Text with
// nothing to change is returned as is, without allocating.
func printable(s string, mode textMode) string {
	if isPlainASCII(s) {
		return s
	}
	if mode != toCopy {
		s = strings.TrimRight(s, "\r\n")
	}
	var b strings.Builder
	b.Grow(len(s))
	// measured is how much of b the column counts; col is the cell column
	// at that point, from the last line break. Graphemes do not span a tab
	// or a break, so measuring the text between them is exact.
	col, measured := 0, 0
	lineBreak := func() {
		if mode == toCopy {
			b.WriteByte('\n')
		} else {
			b.WriteString(breakMark)
		}
		col, measured = 0, b.Len()
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			n, keep := escape(s[i:])
			if keep && mode == toRow {
				b.WriteString(s[i : i+n])
			}
			i += n
			continue
		case c == '\t':
			if mode == toCopy {
				b.WriteByte('\t')
			} else {
				col += ansi.StringWidth(b.String()[measured:])
				n := tabStop - col%tabStop
				b.WriteString(spaces[:n])
				col += n
				measured = b.Len()
			}
		case c == '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			lineBreak()
		case c == '\n':
			lineBreak()
		case c < 0x20 || c == 0x7f:
			// Other C0 controls: bell, backspace, form feed, NUL.
		case c < utf8.RuneSelf:
			b.WriteByte(c)
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			switch {
			case r == utf8.RuneError && n == 1:
				// An invalid byte, such as a raw 8-bit C1 control: a
				// terminal draws it as one cell, the width count as none.
				b.WriteRune(utf8.RuneError)
			case !dropRune(r):
				b.WriteString(s[i : i+n])
			}
			i += n
			continue
		}
		i++
	}
	return b.String()
}

func isPlainASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// dropRune reports a rune the terminal would act on rather than draw: C1
// controls, line and paragraph separators, and the invisible direction and
// width controls (a right-to-left override reverses the rest of the row).
// Joiners (U+200C, U+200D) stay, emoji and some scripts need them.
func dropRune(r rune) bool {
	switch {
	case r >= 0x80 && r < 0xa0,
		r == 0x061c,
		r == 0x200b, r == 0x200e, r == 0x200f,
		r >= 0x2028 && r <= 0x202e,
		r >= 0x2066 && r <= 0x2069,
		r == 0xfeff:
		return true
	}
	return false
}

// escape measures the escape sequence at the start of s (s[0] is ESC) and
// reports whether it is SGR. An unterminated string sequence (OSC, DCS, APC,
// PM, SOS) drops only its introducer, so the text after it stays.
func escape(s string) (n int, sgr bool) {
	if len(s) < 2 {
		return 1, false
	}
	switch s[1] {
	case '[': // CSI: parameters, intermediates, a final byte.
		i := 2
		for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
			i++
		}
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1, s[i] == 'm'
		}
		return i, false
	case ']', 'P', 'X', '^', '_':
		for i := 2; i < len(s); i++ {
			switch {
			case s[i] == 0x07:
				return i + 1, false
			case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
				return i + 2, false
			}
		}
		return 2, false
	}
	// A two-byte (or intermediate-carrying) escape: ESC 7, ESC ( B, ESC c.
	i := 1
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
		return i + 1, false
	}
	return 1, false
}
