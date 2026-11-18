package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// :pulse is a cluster overview built from the all-atespace actors,
// workers, pools and templates datasets. Opening it and each tick refresh
// those datasets in parallel, and the views that share them reuse the rows.

var (
	pulseActors    = viewKey{kActors, ""}
	pulseWorkers   = viewKey{kWorkers, ""}
	pulsePools     = viewKey{kWorkerPools, ""}
	pulseTemplates = viewKey{kTemplates, ""}
	pulseKeys      = map[viewKey]bool{pulseActors: true, pulseWorkers: true, pulsePools: true, pulseTemplates: true}
)

// pulseStates are the actor count columns, in lifecycle order.
var pulseStates = []ateapi.ActorState{
	ateapi.ActorState_ACTOR_STATE_RUNNING,
	ateapi.ActorState_ACTOR_STATE_SUSPENDED,
	ateapi.ActorState_ACTOR_STATE_RESUMING,
	ateapi.ActorState_ACTOR_STATE_SUSPENDING,
	ateapi.ActorState_ACTOR_STATE_REVERTING,
	ateapi.ActorState_ACTOR_STATE_CRASHED,
	ateapi.ActorState_ACTOR_STATE_DELETING,
}

type (
	pulseData struct {
		spaces  []spaceCount // by name, then the total
		pools   []poolUse
		ready   []poolReady
		crashes []pulseActor
		stuck   []pulseActor
		golden  []pulseActor // templates whose golden snapshot failed
		// errs are datasets that failed to load, by section.
		errs map[string]string
		// loading is set until every dataset has loaded once.
		loading bool
	}
	spaceCount struct {
		name   string
		counts map[ateapi.ActorState]int
		total  int
	}
	poolUse struct {
		name                     string
		workers, active          int
		used, slots              int32
		cpu, cpuCap, mem, memCap int64
	}
	poolReady struct {
		id             string
		desired, ready int32
		classes        []string
		unresolved     bool
	}
	pulseActor struct {
		id, state, msg string
		at             time.Time
	}
)

const pulseListMax = 8

// aggregatePulse is the pure part of :pulse.
func aggregatePulse(actors []*ateapi.Actor, workers []*ateapi.Worker, pools []*infrav1.WorkerPool,
	templates []*ateapi.ActorTemplate, now time.Time) pulseData {
	var d pulseData
	spaces := map[string]*spaceCount{}
	total := spaceCount{name: "all", counts: map[ateapi.ActorState]int{}}
	for _, a := range actors {
		md, st := a.GetMetadata(), a.GetStatus()
		sc := spaces[md.GetAtespace()]
		if sc == nil {
			sc = &spaceCount{name: md.GetAtespace(), counts: map[ateapi.ActorState]int{}}
			spaces[md.GetAtespace()] = sc
		}
		sc.counts[st.GetState()]++
		sc.total++
		total.counts[st.GetState()]++
		total.total++
		id := md.GetAtespace() + "/" + md.GetName()
		switch st.GetState() {
		case ateapi.ActorState_ACTOR_STATE_CRASHED:
			d.crashes = append(d.crashes, pulseActor{id: id, msg: st.GetCrash().GetMessage(), at: st.GetCrash().GetCrashTime().AsTime()})
		case ateapi.ActorState_ACTOR_STATE_RESUMING, ateapi.ActorState_ACTOR_STATE_SUSPENDING,
			ateapi.ActorState_ACTOR_STATE_REVERTING, ateapi.ActorState_ACTOR_STATE_DELETING:
			if at := md.GetUpdateTime().AsTime(); now.Sub(at) > explain.StuckAfter {
				d.stuck = append(d.stuck, pulseActor{id: id, state: explain.ActorState(st.GetState()), at: at})
			}
		}
	}
	for _, n := range sortedNames(spaces) {
		d.spaces = append(d.spaces, *spaces[n])
	}
	if len(d.spaces) > 1 {
		d.spaces = append(d.spaces, total)
	}
	// Newest crash first; longest stuck first.
	sort.Slice(d.crashes, func(i, j int) bool { return d.crashes[i].at.After(d.crashes[j].at) })
	sort.Slice(d.stuck, func(i, j int) bool { return d.stuck[i].at.Before(d.stuck[j].at) })

	use := map[string]*poolUse{}
	for _, w := range workers {
		name := dash(w.GetWorkerPool())
		u := use[name]
		if u == nil {
			u = &poolUse{name: name}
			use[name] = u
		}
		st := w.GetStatus()
		u.workers++
		if st.GetState() == ateapi.WorkerState_WORKER_STATE_ACTIVE {
			u.active++
		}
		u.used += st.GetAllocated().GetActors()
		u.slots += st.GetCapacity().GetActors()
		u.cpu += st.GetAllocated().GetCpuMillicores()
		u.cpuCap += st.GetCapacity().GetCpuMillicores()
		u.mem += st.GetAllocated().GetMemoryBytes()
		u.memCap += st.GetCapacity().GetMemoryBytes()
	}
	for _, n := range sortedNames(use) {
		d.pools = append(d.pools, *use[n])
	}

	for _, p := range pools {
		r := poolReady{id: p.Namespace + "/" + p.Name, desired: p.Spec.Replicas, ready: p.Status.ReadyReplicas}
		for _, sc := range p.Status.SandboxClasses {
			name := string(sc.Name)
			for _, c := range sc.Conditions {
				if c.Type == infrav1.WorkerPoolSandboxClassConditionResolved && c.Status != metav1.ConditionTrue {
					r.unresolved = true
					name += "!"
				}
			}
			r.classes = append(r.classes, name)
		}
		d.ready = append(d.ready, r)
	}
	sort.Slice(d.ready, func(i, j int) bool { return d.ready[i].id < d.ready[j].id })

	for _, t := range templates {
		if msg := t.GetStatus().GetGoldenSnapshotStatus().GetErrorMessage(); msg != "" {
			md := t.GetMetadata()
			d.golden = append(d.golden, pulseActor{id: md.GetAtespace() + "/" + md.GetName(), msg: msg})
		}
	}
	sort.Slice(d.golden, func(i, j int) bool { return d.golden[i].id < d.golden[j].id })
	return d
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *Model) openPulse() tea.Cmd {
	p := m.newPager(stTitleKind.Render("pulse"))
	p.raw = true
	m.pulse = &pulseData{loading: true}
	p.reload = func() tea.Cmd {
		return tea.Batch(m.fetch(pulseActors), m.fetch(pulseWorkers), m.fetch(pulsePools), m.fetch(pulseTemplates))
	}
	p.resize = m.drawPulse
	m.drawPulse()
	return tea.Batch(m.pulseRefresh(), p.reload())
}

type pulseMsg struct {
	gen  int
	data pulseData
}

// pulseRefresh aggregates the cached datasets off the UI goroutine. Only
// the object pointers are copied here; rows are never mutated in place.
func (m *Model) pulseRefresh() tea.Cmd {
	if m.pager == nil || m.pulse == nil {
		return nil
	}
	errs := map[string]string{}
	loading := false
	for name, k := range map[string]viewKey{"actors": pulseActors, "workers": pulseWorkers, "pools": pulsePools, "templates": pulseTemplates} {
		d := m.data[k]
		switch {
		case d == nil || (d.fetchedAt.IsZero() && d.err == nil):
			loading = true
		case d.err != nil && len(d.rows) == 0:
			errs[name] = errString(d.err)
		}
	}
	actors := cachedRows[*ateapi.Actor](m, pulseActors)
	workers := cachedRows[*ateapi.Worker](m, pulseWorkers)
	pools := cachedRows[*infrav1.WorkerPool](m, pulsePools)
	templates := cachedRows[*ateapi.ActorTemplate](m, pulseTemplates)
	gen := m.pager.gen
	return func() tea.Msg {
		d := aggregatePulse(actors, workers, pools, templates, time.Now())
		d.errs, d.loading = errs, loading
		return pulseMsg{gen: gen, data: d}
	}
}

// cachedRows is the objects a dataset holds, stale or not.
func cachedRows[T any](m *Model, k viewKey) []T {
	d := m.data[k]
	if d == nil {
		return nil
	}
	out := make([]T, 0, len(d.rows))
	for i := range d.rows {
		if o, ok := d.rows[i].obj.(T); ok {
			out = append(out, o)
		}
	}
	return out
}

func (m *Model) pulseLoaded(msg pulseMsg) {
	if m.pager == nil || m.pulse == nil || msg.gen != m.pager.gen {
		return
	}
	*m.pulse = msg.data
	m.pager.loading = false
	m.drawPulse()
}

func (m *Model) drawPulse() {
	if m.pager == nil || m.pulse == nil {
		return
	}
	m.pager.lines = pulseLines(m.pulse, m.width)
	m.pager.scroll(0, m.pulseHeight())
}

// pulseHeight is the body height; the pulse boxes have no outer border.
func (m *Model) pulseHeight() int { return m.pagerHeight() + 2 }

// pulseSection is one box of the overview.
type pulseSection struct {
	title string
	lines []string
}

// pulseLines lays the sections out in boxes: two columns when wide.
func pulseLines(d *pulseData, width int) []string {
	colW := width
	if width >= 110 {
		colW = width / 2
	}
	left := []pulseSection{
		{"Actors by atespace", actorCountLines(d, colW-4)},
		{"Workers", poolUseLines(d)},
		{"WorkerPools", poolReadyLines(d)},
	}
	right := []pulseSection{
		{"Recent crashes", pulseList(d, "actors", d.crashes, func(a pulseActor, now time.Time) string {
			return fg(cRed).Render(a.id) + stDim.Render("  "+explain.Age(a.at, now)+" ago") + "\n  " + fg(cText).Render(a.msg)
		})},
		{fmt.Sprintf("Stuck over %dm", int(explain.StuckAfter.Minutes())), pulseList(d, "actors", d.stuck, func(a pulseActor, now time.Time) string {
			return fg(cYellow).Render(a.id) + stDim.Render("  "+a.state+" for "+explain.Age(a.at, now))
		})},
		{"Golden snapshot errors", pulseList(d, "templates", d.golden, func(a pulseActor, _ time.Time) string {
			return fg(cRed).Render(a.id) + "\n  " + fg(cText).Render(a.msg)
		})},
	}
	if width < 110 {
		return stackBoxes(append(left, right...), width)
	}
	lw := width / 2
	l, r := stackBoxes(left, lw), stackBoxes(right, width-lw)
	out := make([]string, max(len(l), len(r)))
	for i := range out {
		a, b := "", ""
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			b = r[i]
		}
		out[i] = fit(a, lw) + b
	}
	return out
}

func stackBoxes(ss []pulseSection, width int) []string {
	var out []string
	for _, s := range ss {
		out = append(out, box(stSection.Render(s.title), s.lines, width, max(len(s.lines), 1), stTableBorder)...)
	}
	return out
}

func (d *pulseData) status(section string) (string, bool) {
	if e, ok := d.errs[section]; ok {
		return stStatusErr.Render(e), true
	}
	if d.loading {
		return stDim.Render("loading…"), true
	}
	return "", false
}

var stateStyle = map[ateapi.ActorState]lipgloss.Style{
	ateapi.ActorState_ACTOR_STATE_RUNNING:    fg(cGreen),
	ateapi.ActorState_ACTOR_STATE_SUSPENDED:  fg(cSubtext0),
	ateapi.ActorState_ACTOR_STATE_RESUMING:   fg(cPeach),
	ateapi.ActorState_ACTOR_STATE_SUSPENDING: fg(cPeach),
	ateapi.ActorState_ACTOR_STATE_REVERTING:  fg(cPeach),
	ateapi.ActorState_ACTOR_STATE_CRASHED:    fg(cRed).Bold(true),
	ateapi.ActorState_ACTOR_STATE_DELETING:   fg(cOverlay0),
}

// shortStates label the count columns when the full names do not fit.
var shortStates = map[ateapi.ActorState]string{
	ateapi.ActorState_ACTOR_STATE_RUNNING:    "RUN",
	ateapi.ActorState_ACTOR_STATE_SUSPENDED:  "SUSP",
	ateapi.ActorState_ACTOR_STATE_RESUMING:   "RESUM",
	ateapi.ActorState_ACTOR_STATE_SUSPENDING: "SUSPG",
	ateapi.ActorState_ACTOR_STATE_REVERTING:  "REVRT",
	ateapi.ActorState_ACTOR_STATE_CRASHED:    "CRASH",
	ateapi.ActorState_ACTOR_STATE_DELETING:   "DEL",
}

func actorCountLines(d *pulseData, width int) []string {
	if s, ok := d.status("actors"); ok && len(d.spaces) == 0 {
		return []string{s}
	}
	if len(d.spaces) == 0 {
		return []string{stDim.Render("no actors")}
	}
	nameW := len("ATESPACE")
	for _, s := range d.spaces {
		nameW = max(nameW, len(s.name))
	}
	// Columns that are zero everywhere are left out, except the usual three.
	var states []ateapi.ActorState
	for _, st := range pulseStates {
		show := st == ateapi.ActorState_ACTOR_STATE_RUNNING || st == ateapi.ActorState_ACTOR_STATE_SUSPENDED ||
			st == ateapi.ActorState_ACTOR_STATE_CRASHED
		for _, s := range d.spaces {
			show = show || s.counts[st] > 0
		}
		if show {
			states = append(states, st)
		}
	}
	labels := make([]string, len(states))
	full := nameW + 6
	for i, st := range states {
		labels[i] = strings.ToUpper(explain.ActorState(st))
		full += max(len(labels[i]), 4) + 1
	}
	if full > width {
		for i, st := range states {
			labels[i] = shortStates[st]
		}
	}
	head := fmt.Sprintf("%-*s", nameW, "ATESPACE")
	widths := make([]int, len(states))
	for i, label := range labels {
		widths[i] = max(len(label), 4)
		head += fmt.Sprintf(" %*s", widths[i], label)
	}
	head += fmt.Sprintf(" %5s", "TOTAL")
	out := []string{stColHeader.Render(head)}
	for _, s := range d.spaces {
		line := fg(cBlue).Render(fmt.Sprintf("%-*s", nameW, s.name))
		if s.name == "all" {
			line = stSection.Render(fmt.Sprintf("%-*s", nameW, s.name))
		}
		for i, st := range states {
			if n := s.counts[st]; n == 0 {
				line += stDim.Render(fmt.Sprintf(" %*s", widths[i], "·"))
			} else {
				line += stateStyle[st].Render(fmt.Sprintf(" %*d", widths[i], n))
			}
		}
		line += stCount.Render(fmt.Sprintf(" %5d", s.total))
		out = append(out, line)
	}
	return out
}

// bar draws used/total as a 10-cell gauge, green to red as it fills.
func bar(used, total int64) string {
	const cells = 10
	if total <= 0 {
		return stDim.Render(strings.Repeat("░", cells))
	}
	n := int(used * cells / total)
	n = min(max(n, 0), cells)
	if used > 0 && n == 0 {
		n = 1
	}
	c := cGreen
	switch pct := used * 100 / total; {
	case pct >= 100:
		c = cRed
	case pct >= 80:
		c = cYellow
	}
	return fg(c).Render(strings.Repeat("█", n)) + stDim.Render(strings.Repeat("░", cells-n))
}

func poolUseLines(d *pulseData) []string {
	if s, ok := d.status("workers"); ok && len(d.pools) == 0 {
		return []string{s}
	}
	if len(d.pools) == 0 {
		return []string{stDim.Render("no workers registered")}
	}
	nameW := len("POOL")
	for _, p := range d.pools {
		nameW = max(nameW, len(p.name))
	}
	out := []string{stColHeader.Render(fmt.Sprintf("%-*s %6s  %-18s %9s %13s", nameW, "POOL", "ACTIVE", "SLOTS", "CPU", "MEM"))}
	for _, p := range d.pools {
		active := fmt.Sprintf("%6s", fmt.Sprintf("%d/%d", p.active, p.workers))
		as := fg(cGreen).Render(active)
		if p.active < p.workers {
			as = fg(cYellow).Render(active)
		}
		out = append(out, fg(cBlue).Render(fmt.Sprintf("%-*s", nameW, p.name))+" "+as+"  "+
			bar(int64(p.used), int64(p.slots))+stCount.Render(fmt.Sprintf(" %7s", fmt.Sprintf("%d/%d", p.used, p.slots)))+
			stCount.Render(fmt.Sprintf(" %9s", explain.Millicores(p.cpu)+"/"+explain.Millicores(p.cpuCap)))+
			stCount.Render(fmt.Sprintf(" %13s", explain.Bytes(p.mem)+"/"+explain.Bytes(p.memCap))))
	}
	return out
}

func poolReadyLines(d *pulseData) []string {
	if s, ok := d.status("pools"); ok && len(d.ready) == 0 {
		return []string{s}
	}
	if len(d.ready) == 0 {
		return []string{stDim.Render("no WorkerPools")}
	}
	nameW := len("WORKERPOOL")
	for _, p := range d.ready {
		nameW = max(nameW, len(p.id))
	}
	out := []string{stColHeader.Render(fmt.Sprintf("%-*s %7s  %s", nameW, "WORKERPOOL", "READY", "SANDBOX"))}
	for _, p := range d.ready {
		ready := fmt.Sprintf("%7s", fmt.Sprintf("%d/%d", p.ready, p.desired))
		rs := fg(cGreen).Render(ready)
		if p.ready < p.desired {
			rs = fg(cPeach).Render(ready)
		}
		sb := fg(cTeal).Render(dash(strings.Join(p.classes, ",")))
		if p.unresolved {
			sb = fg(cRed).Render(strings.Join(p.classes, ",") + "  not resolved")
		}
		out = append(out, fg(cBlue).Render(fmt.Sprintf("%-*s", nameW, p.id))+" "+rs+"  "+sb)
	}
	return out
}

func pulseList(d *pulseData, section string, items []pulseActor, render func(pulseActor, time.Time) string) []string {
	if s, ok := d.status(section); ok && len(items) == 0 {
		return []string{s}
	}
	if len(items) == 0 {
		return []string{levelGlyph[explain.OK] + " " + fg(cGreen).Render("none")}
	}
	now := time.Now()
	var out []string
	for i, a := range items {
		if i == pulseListMax {
			out = append(out, stDim.Render(fmt.Sprintf("… and %d more", len(items)-i)))
			break
		}
		out = append(out, strings.Split(render(a, now), "\n")...)
	}
	return out
}
