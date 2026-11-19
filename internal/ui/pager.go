package ui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"
)

// pager is a scrollable text view over the table area.
type pager struct {
	title   string
	lines   []string
	offset  int
	follow  bool
	loading bool
	gen     int
	// reload refreshes the content on each tick; nil for static views.
	reload func() tea.Cmd
	// resize redraws width-dependent content.
	resize func()
	// raw lines are drawn full width with no box around them (:pulse).
	raw bool
	// items make the pager a list with a cursor (relations); nil otherwise.
	items  []relation
	cursor int
	// forwards makes the pager the :pf list, drawn from Model.forwards;
	// contexts the :ctx list.
	forwards bool
	contexts bool
}

type pagerMsg struct {
	gen   int
	lines []string
	err   error
}

func (p *pager) setLines(lines []string, h int) {
	p.lines = lines
	if p.follow {
		p.offset = len(p.lines)
	}
	p.scroll(0, h)
}

func (p *pager) scroll(n, h int) {
	p.offset += n
	if max := len(p.lines) - h; p.offset > max {
		p.offset = max
	}
	if p.offset < 0 {
		p.offset = 0
	}
	// Scrolling up pauses log follow; reaching the end resumes it.
	if n < 0 {
		p.follow = false
	}
}

func (m *Model) newPager(title string) *pager {
	m.closePager()
	m.gen++
	m.pager = &pager{title: title, gen: m.gen, loading: true, lines: []string{stDim.Render("loading…")}}
	m.mode = modePager
	return m.pager
}

func (m *Model) closePager() {
	if m.logs != nil {
		m.logs.stop()
		m.logs = nil
	}
	m.pager, m.pulse = nil, nil
}

// lookup finds the freshest copy of a row so live views follow updates.
func (m *Model) lookup(k viewKey, id string) any {
	if d := m.data[k]; d != nil {
		for i := range d.rows {
			if d.rows[i].id == id {
				return d.rows[i].obj
			}
		}
	}
	return nil
}

func (m *Model) openYAML(k viewKey, id string) tea.Cmd {
	p := m.newPager(stTitleKind.Render("yaml") + " " + stTitleCount.Render(id))
	gen := p.gen
	p.reload = func() tea.Cmd {
		obj := m.lookup(k, id)
		if obj == nil {
			return nil
		}
		return func() tea.Msg {
			lines, err := toYAML(obj)
			return pagerMsg{gen: gen, lines: lines, err: err}
		}
	}
	return p.reload()
}

func toYAML(obj any) ([]string, error) {
	var out []byte
	var err error
	switch o := obj.(type) {
	case proto.Message:
		var j []byte
		j, err = protojson.MarshalOptions{UseProtoNames: true}.Marshal(o)
		if err == nil {
			out, err = yaml.JSONToYAML(j)
		}
	case *infrav1.WorkerPool:
		c := o.DeepCopy()
		c.ManagedFields = nil
		out, err = yaml.Marshal(c)
	case *infrav1.SandboxConfig:
		c := o.DeepCopy()
		c.ManagedFields = nil
		out, err = yaml.Marshal(c)
	case *infrav1.CSIDriverConfig:
		c := o.DeepCopy()
		c.ManagedFields = nil
		out, err = yaml.Marshal(c)
	default:
		err = fmt.Errorf("no yaml for %T", obj)
	}
	if err != nil {
		return nil, err
	}
	raw := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = colorYAML(l)
	}
	return lines, nil
}

func colorYAML(l string) string {
	trimmed := strings.TrimLeft(l, " ")
	indent := l[:len(l)-len(trimmed)]
	dash := ""
	if strings.HasPrefix(trimmed, "- ") {
		dash, trimmed = stYAMLDash.Render("- "), trimmed[2:]
	}
	if i := strings.Index(trimmed, ":"); i > 0 && !strings.ContainsAny(trimmed[:i], " \"'") {
		return indent + dash + stYAMLKey.Render(trimmed[:i+1]) + stYAMLVal.Render(trimmed[i+1:])
	}
	return indent + dash + stYAMLVal.Render(trimmed)
}

func (m *Model) openExplain(k viewKey, id string) tea.Cmd {
	p := m.newPager(stTitleKind.Render("explain") + " " + stTitleCount.Render(id))
	gen := p.gen
	b := m.backend
	p.reload = func() tea.Cmd {
		obj := m.lookup(k, id)
		if obj == nil || b == nil {
			return nil
		}
		m.pager.loading = true
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			r, err := buildReport(ctx, b, obj)
			if err != nil {
				return pagerMsg{gen: gen, err: err}
			}
			return pagerMsg{gen: gen, lines: renderReport(r)}
		}
	}
	return p.reload()
}

// buildReport fetches what an explanation needs, in parallel.
func buildReport(ctx context.Context, b Backend, obj any) (explain.Report, error) {
	now := time.Now()
	switch o := obj.(type) {
	case *ateapi.Actor:
		in := explain.ActorInput{Actor: o, Now: now}
		var wg sync.WaitGroup
		// Events are optional: a failure leaves them out of the report.
		if wa := o.GetStatus().GetWorkerAssignment(); wa.GetWorkerPod() != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in.PodEvents, _ = b.PodEvents(ctx, wa.GetWorkerNamespace(), wa.GetWorkerPod())
			}()
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			in.Template, in.TemplateErr = b.GetActorTemplate(ctx, o.GetActorTemplate())
		}()
		go func() {
			defer wg.Done()
			in.Workers, _ = b.ListWorkers(ctx)
		}()
		wg.Wait()
		return explain.Actor(in), nil
	case *ateapi.Worker:
		in := explain.WorkerInput{Worker: o, Now: now}
		var err error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			in.Assignments, err = b.ListWorkerAssignments(ctx, o.GetMetadata().GetName())
		}()
		go func() {
			defer wg.Done()
			if o.GetWorkerPod() != "" {
				in.PodEvents, _ = b.PodEvents(ctx, o.GetWorkerNamespace(), o.GetWorkerPod())
			}
		}()
		wg.Wait()
		if err != nil {
			return explain.Report{}, err
		}
		return explain.Worker(in), nil
	case *ateapi.ActorTemplate:
		as, err := b.ListActors(ctx, o.GetMetadata().GetAtespace())
		if err != nil {
			return explain.Report{}, err
		}
		return explain.Template(explain.TemplateInput{Template: o, Actors: as, Now: now}), nil
	case *infrav1.WorkerPool:
		ws, _ := b.ListWorkers(ctx)
		return explain.WorkerPool(o, ws, now), nil
	case *infrav1.SandboxConfig:
		ps, err := b.ListWorkerPools(ctx)
		if err != nil {
			return explain.SandboxConfig(o, nil, now), nil
		}
		pools := make([]*infrav1.WorkerPool, len(ps))
		for i := range ps {
			pools[i] = &ps[i]
		}
		return explain.SandboxConfig(o, pools, now), nil
	}
	return explain.Report{}, fmt.Errorf("explain is not available for %T", obj)
}

// levelGlyph and levelStyle are set by ApplySkin.
var (
	levelGlyph map[explain.Level]string
	levelStyle map[explain.Level]func(...string) string
)

func renderReport(r explain.Report) []string {
	var out []string
	out = append(out, stSection.Render(r.Subject)+"  "+levelStyle[r.Level](r.Summary), "")
	w := 0
	for _, f := range r.Facts {
		w = max(w, len(f.Label))
	}
	for _, f := range r.Facts {
		out = append(out, stLabel.Render(fmt.Sprintf("%-*s", w+2, f.Label))+stCount.Render(f.Value))
	}
	out = append(out, "", stSection.Render("Diagnosis"))
	for _, f := range r.Findings {
		out = append(out, levelGlyph[f.Level]+" "+levelStyle[f.Level](f.Title))
		for _, d := range f.Detail {
			out = append(out, "    "+stDim.Render(d))
		}
	}
	if len(r.Next) > 0 {
		out = append(out, "", stSection.Render("Next"))
		for _, n := range r.Next {
			out = append(out, stDim.Render("$ ")+fg(cSky).Render(n))
		}
	}
	return out
}

func (m *Model) openTimeline(id string) {
	title := "timeline"
	if id != "" {
		title += " " + stTitleCount.Render(id)
	}
	p := m.newPager(stTitleKind.Render(title))
	p.follow = true
	build := func() []string {
		var out []string
		for _, e := range m.timeline {
			if id != "" && e.id != id {
				continue
			}
			change := fg(cGreen).Render("seen ") + e.to
			if e.from != "" {
				change = e.from + stDim.Render(" → ") + e.to
			}
			line := stLogTime.Render(e.at.Format("15:04:05")) + "  "
			if id == "" {
				line += fg(cBlue).Render(fmt.Sprintf("%-40s", e.id)) + " "
			}
			line += change
			if e.worker != "-" {
				line += stDim.Render("  on " + e.worker)
			}
			out = append(out, line)
		}
		if len(out) == 0 {
			out = []string{stDim.Render("no changes observed yet; the timeline records what each poll sees while ate-tui runs")}
		}
		return out
	}
	p.loading = false
	p.setLines(build(), m.pagerHeight())
	gen := p.gen
	p.reload = func() tea.Cmd {
		lines := build()
		return func() tea.Msg { return pagerMsg{gen: gen, lines: lines} }
	}
}

func (m *Model) openHelp() {
	p := m.newPager(stTitleKind.Render("help"))
	p.loading = false
	sections := []struct {
		name  string
		hints []hint
	}{
		{"Navigation", []hint{
			{":<kind> [atespace]", "switch view (actors, workers, templates, workerpools, sandboxconfigs, csidriverconfigs, atespaces, tags)"},
			{":atespace <name>", "scope to an atespace; 0 or :all for every atespace"},
			{"tab / shift-tab", "cycle common views"},
			{"[ / ]", "back / forward through view history"},
			{"j k g G", "move; ctrl-f / ctrl-b page"},
			{"/", "filter rows (space = AND, !term = NOT); esc clears"},
			{"ctrl-z", "toggle faults only (crashed, stuck, draining, not ready)"},
			{"S / I", "cycle sort column / invert"},
			{"r", "refresh now (or reconnect)"},
			{":ctx [name]", "switch kube context and reconnect; :ctx alone lists them"},
			{"w", "wide: extra columns (pod, sandbox, crash, uid, …), none hidden"},
			{"c", "copy the row's name, or a pager's text (C in logs), with OSC 52"},
			{"q, ctrl-c", "quit"},
		}},
		{"Actors", []hint{
			{"⏎ / X", "explain: state, crash, snapshots, placement, template"},
			{"y", "yaml"},
			{"l", "follow logs across worker moves, like kubectl ate logs -f"},
			{"e", "egress policy: rules, hosts, ports, injected header names"},
			{"T", "timeline of state changes seen by ate-tui; :timeline for all"},
			{"o / t", "jump to worker / template"},
		}},
		{"Logs", []hint{
			{"f", "toggle follow; scrolling up pauses it"},
			{"/  n  N", "search, next / previous match; esc clears"},
			{"c", "source: all → containers → lifecycle"},
			{"w", "toggle line wrap"},
		}},
		{"Workers & pools", []hint{
			{"⏎", "worker: actor assignments · pool: its workers"},
			{"l", "worker: actors its pod kept logs for, deleted ones too; ⏎ reads them"},
			{"X", "explain capacity, epochs, sandbox config, pod Warning events"},
			{"p", "worker: jump to its pool"},
		}},
		{"ate CRDs (workerpools, sandboxconfigs, csidriverconfigs)", []hint{
			{"⏎", "pool: its workers · sandboxconfig: the pools using it · csidriverconfig: yaml"},
			{"X", "pool: rollout and sandbox class · sandboxconfig: versions, pools, deletion"},
			{"E", "Kubernetes events about the object"},
		}},
		{"Diagnosis", []hint{
			{"u", "relations of any row: template, tags, worker, pool, node, actors; ⏎ jumps"},
			{"E", "Kubernetes events of the worker pod (actor or worker), or of an ate CRD"},
			{":pulse", "cluster overview: actor states, slots, pools, crashes, stuck, golden errors"},
			{":access", "global and atespace access policy bindings (no permission prediction)"},
		}},
		{"Proxy (needs --write or writeContexts; resumes a suspended actor)", []hint{
			{"f", "actor: forward local ports through atenet-router, like kubectl ate proxy actor"},
			{":pf", "list forwards with connections and bytes; ⏎ actor, ctrl-d stop"},
		}},
		{"Actions (need --write or writeContexts in the config)", []hint{
			{"space", "mark / unmark a row; actions apply to marked rows, or the current one"},
			{"s", "actor: suspend (tab in the dialog: async or sync durability)"},
			{"R", "actor: resume"},
			{"v", "actor: revert to its durable snapshot (clears a crash)"},
			{"ctrl-d", "actor: delete; type the name (or yes for several) to confirm"},
			{"D", "worker: drain (one-way)"},
			{"A / U", "worker: mark available / unavailable"},
			{"n", "atespace, template, actor, tag or ate CRD: create from a manifest in $EDITOR"},
			{"ctrl-n", "create a copy of the selected object (server fields removed)"},
			{"ctrl-e", "ate CRD: edit in $EDITOR; the dialog shows the diff, a conflict is refused"},
			{"z", "workerpool: scale to a number of replicas"},
			{"ctrl-d", "ate CRD: delete; type the name to confirm (kubeconfig RBAC applies)"},
			{":journal", "actions and results, this session and earlier"},
		}},
	}
	if c := m.keys.custom(); len(c) > 0 {
		sections = append([]struct {
			name  string
			hints []hint
		}{{"Rebound in config.yaml (the tables below show the defaults)", c}}, sections...)
	}
	var lines []string
	for _, s := range sections {
		lines = append(lines, stSection.Render(s.name))
		for _, h := range s.hints {
			lines = append(lines, "  "+stHintKey.Render(fmt.Sprintf("%-22s", h.key))+stHintDesc.Render(h.desc))
		}
		lines = append(lines, "")
	}
	p.setLines(lines, m.pagerHeight())
}
