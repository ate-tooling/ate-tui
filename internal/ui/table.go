package ui

import (
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

type row struct {
	id     string
	obj    any
	level  rowLevel
	cells  []string
	sortBy map[int]string
	// search is the lowercased cells, built once so filtering per keystroke
	// does no allocation.
	search string
}

func newRow(id string, obj any, lv rowLevel, cells []string, sortBy map[int]string) row {
	return row{id: id, obj: obj, level: lv, cells: cells, sortBy: sortBy, search: strings.ToLower(strings.Join(cells, " "))}
}

func (r row) sortKey(col int) string {
	if k, ok := r.sortBy[col]; ok {
		return k
	}
	if col < len(r.cells) {
		return r.cells[col]
	}
	return ""
}

// dataset is one cached list, keyed by kind and scope.
type dataset struct {
	rows      []row
	err       error
	fetchedAt time.Time
	loading   bool
	gen       int
}

// tableState is the cursor, filter and sort for the visible table. It is
// rebuilt from the dataset whenever data, filter or sort changes.
type tableState struct {
	visible  []int // indexes into dataset rows
	cursor   int
	offset   int
	filter   string
	faults   bool
	sortCol  int
	sortDesc bool
	marked   map[string]bool
	widths   []int
}

// filterTerms are space-separated AND terms; a leading ! negates one.
func matches(r *row, terms []string) bool {
	for _, t := range terms {
		neg := strings.HasPrefix(t, "!")
		if neg {
			t = t[1:]
		}
		if t == "" {
			continue
		}
		if strings.Contains(r.search, t) == neg {
			return false
		}
	}
	return true
}

func (ts *tableState) rebuild(rows []row, columns []string, keepID string) {
	terms := strings.Fields(strings.ToLower(ts.filter))
	ts.visible = ts.visible[:0]
	for i := range rows {
		if ts.faults && rows[i].level < lvWarn {
			continue
		}
		if matches(&rows[i], terms) {
			ts.visible = append(ts.visible, i)
		}
	}
	col, desc := ts.sortCol, ts.sortDesc
	sort.SliceStable(ts.visible, func(a, b int) bool {
		ka, kb := rows[ts.visible[a]].sortKey(col), rows[ts.visible[b]].sortKey(col)
		if ka == kb {
			return rows[ts.visible[a]].id < rows[ts.visible[b]].id
		}
		return (ka < kb) != desc
	})

	ts.widths = make([]int, len(columns))
	for i, c := range columns {
		// +1 leaves room for the sort arrow.
		ts.widths[i] = runewidth.StringWidth(c) + 1
	}
	for _, vi := range ts.visible {
		for i, c := range rows[vi].cells {
			if i < len(ts.widths) {
				if w := runewidth.StringWidth(c); w > ts.widths[i] {
					ts.widths[i] = w
				}
			}
		}
	}

	ts.cursor = 0
	if keepID != "" {
		for i, vi := range ts.visible {
			if rows[vi].id == keepID {
				ts.cursor = i
				break
			}
		}
	}
	ts.clamp(0)
}

func (ts *tableState) clamp(height int) {
	if ts.cursor >= len(ts.visible) {
		ts.cursor = len(ts.visible) - 1
	}
	if ts.cursor < 0 {
		ts.cursor = 0
	}
	if height <= 0 {
		return
	}
	if ts.cursor < ts.offset {
		ts.offset = ts.cursor
	}
	if ts.cursor >= ts.offset+height {
		ts.offset = ts.cursor - height + 1
	}
	if max := len(ts.visible) - height; ts.offset > max {
		ts.offset = max
	}
	if ts.offset < 0 {
		ts.offset = 0
	}
}

const (
	maxColWidth = 48
	colGap      = 2
)

// layoutWidths fits column widths into avail. It first hides the columns in
// drop order (width 0), then shrinks the widest column.
func layoutWidths(natural []int, avail int, drop []int) []int {
	w := make([]int, len(natural))
	total := 0
	for i, n := range natural {
		w[i] = min(n, maxColWidth)
		total += w[i] + colGap
	}
	var dropped []int
	for _, d := range drop {
		if total <= avail {
			break
		}
		if d < len(w) {
			total -= w[d] + colGap
			dropped = append(dropped, d)
		}
	}
	for _, d := range dropped {
		w[d] = 0
	}
	// Bring back the most important dropped columns that fit after all.
	for i := len(dropped) - 1; i >= 0; i-- {
		d := dropped[i]
		if n := min(natural[d], maxColWidth); total+n+colGap <= avail {
			w[d] = n
			total += n + colGap
		}
	}
	for total > avail {
		widest := 0
		for i := range w {
			if w[i] > w[widest] {
				widest = i
			}
		}
		if w[widest] <= 6 {
			break
		}
		w[widest]--
		total--
	}
	return w
}

func renderCells(cells []string, widths []int, gutter string) string {
	var b strings.Builder
	b.WriteString(gutter)
	pad := 0
	for i, wd := range widths {
		if wd == 0 {
			continue
		}
		c := ""
		if i < len(cells) {
			c = cells[i]
		}
		if runewidth.StringWidth(c) > wd {
			c = ansi.Truncate(c, wd, "…")
		}
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(c)
		pad = wd - runewidth.StringWidth(c) + colGap
	}
	return b.String()
}
