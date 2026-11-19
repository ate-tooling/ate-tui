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
)

// relation is one row of the `u` pager: an object related to the selected
// one, and where ⏎ jumps to show it.
type relation struct {
	rel  string // how it relates: "template", "worker", …
	kind kind
	// scope is the atespace a scoped kind is shown in; filter narrows the
	// table and id is the row the cursor lands on.
	scope, filter, id string
	name              string
	info              string
	level             rowLevel
}

type relationsMsg struct {
	gen   int
	items []relation
	err   error
}

// relCache is what the cached datasets already hold, copied on the UI
// goroutine so the background build reads no shared state. A nil slice
// means the dataset is not loaded and the build fetches it.
type relCache struct {
	templates []*ateapi.ActorTemplate
	workers   []*ateapi.Worker
	actors    []*ateapi.Actor
	pools     []*infrav1.WorkerPool
}

// cachedObjs returns the objects of a loaded dataset, or nil.
func cachedObjs[T any](m *Model, keys ...viewKey) []T {
	for _, k := range keys {
		if d := m.data[k]; d != nil && !d.fetchedAt.IsZero() && d.err == nil {
			return cachedRows[T](m, k)
		}
	}
	return nil
}

func (m *Model) openRelations(r *row) tea.Cmd {
	p := m.newPager(stTitleKind.Render("relations") + " " + stTitleCount.Render(r.id))
	p.items = []relation{}
	var c relCache
	switch o := r.obj.(type) {
	case *ateapi.Actor:
		as := o.GetMetadata().GetAtespace()
		c.templates = cachedObjs[*ateapi.ActorTemplate](m, viewKey{kTemplates, as}, viewKey{kTemplates, ""})
		c.workers = cachedObjs[*ateapi.Worker](m, viewKey{kWorkers, ""})
	case *ateapi.ActorTemplate:
		as := o.GetMetadata().GetAtespace()
		c.actors = cachedObjs[*ateapi.Actor](m, viewKey{kActors, as}, viewKey{kActors, ""})
	case *infrav1.WorkerPool:
		c.workers = cachedObjs[*ateapi.Worker](m, viewKey{kWorkers, ""})
	case *infrav1.SandboxConfig:
		c.pools = cachedObjs[*infrav1.WorkerPool](m, viewKey{kWorkerPools, ""})
	case *ateapi.Worker:
		c.pools = cachedObjs[*infrav1.WorkerPool](m, viewKey{kWorkerPools, ""})
	}
	gen, b, obj := p.gen, m.backend, r.obj
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		items, err := buildRelations(ctx, b, obj, c)
		return relationsMsg{gen: gen, items: items, err: err}
	}
}

// buildRelations fetches what the cache lacks, in parallel, then lists
// the relations.
func buildRelations(ctx context.Context, b Backend, obj any, c relCache) ([]relation, error) {
	switch o := obj.(type) {
	case *ateapi.Actor:
		var wg sync.WaitGroup
		tpl := findTemplate(c.templates, o.GetActorTemplate())
		if tpl == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tpl, _ = b.GetActorTemplate(ctx, o.GetActorTemplate())
			}()
		}
		if c.workers == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.workers, _ = b.ListWorkers(ctx)
			}()
		}
		wg.Wait()
		return actorRelations(o, tpl, c.workers), nil
	case *ateapi.Worker:
		var as []*ateapi.ActorAssignment
		var err error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			as, err = b.ListWorkerAssignments(ctx, o.GetMetadata().GetName())
		}()
		if c.pools == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ps, _ := b.ListWorkerPools(ctx)
				for i := range ps {
					c.pools = append(c.pools, &ps[i])
				}
			}()
		}
		wg.Wait()
		if err != nil {
			return nil, err
		}
		return workerRelations(o, as, c.pools), nil
	case *ateapi.ActorTemplate:
		if c.actors == nil {
			as, err := b.ListActors(ctx, o.GetMetadata().GetAtespace())
			if err != nil {
				return nil, err
			}
			c.actors = as
		}
		return templateRelations(o, c.actors), nil
	case *infrav1.WorkerPool:
		if c.workers == nil {
			ws, err := b.ListWorkers(ctx)
			if err != nil {
				return nil, err
			}
			c.workers = ws
		}
		return poolRelations(o, c.workers), nil
	case *infrav1.SandboxConfig:
		if c.pools == nil {
			ps, err := b.ListWorkerPools(ctx)
			if err != nil {
				return nil, err
			}
			for i := range ps {
				c.pools = append(c.pools, &ps[i])
			}
		}
		return sandboxConfigRelations(o, c.pools), nil
	case *ateapi.Atespace:
		n := o.GetMetadata().GetName()
		return []relation{
			{rel: "actors", kind: kActors, scope: n, name: n, info: "actors in this atespace"},
			{rel: "templates", kind: kTemplates, scope: n, name: n, info: "templates in this atespace"},
			{rel: "tags", kind: kTags, scope: n, name: n, info: "tags in this atespace"},
		}, nil
	case *ateapi.ActorAssignment:
		return []relation{actorRel("actor", o.GetActor(), "")}, nil
	case *ateapi.Tag:
		var out []relation
		if a := o.GetSourceActor(); a.GetName() != "" {
			out = append(out, actorRel("source actor", a, ""))
		}
		return out, nil
	}
	return nil, fmt.Errorf("no relations for %T", obj)
}

func findTemplate(ts []*ateapi.ActorTemplate, ref *ateapi.ObjectRef) *ateapi.ActorTemplate {
	for _, t := range ts {
		if t.GetMetadata().GetAtespace() == ref.GetAtespace() && t.GetMetadata().GetName() == ref.GetName() {
			return t
		}
	}
	return nil
}

func findWorker(ws []*ateapi.Worker, name string) *ateapi.Worker {
	for _, w := range ws {
		if w.GetMetadata().GetName() == name {
			return w
		}
	}
	return nil
}

func refID(r *ateapi.ObjectRef) string { return r.GetAtespace() + "/" + r.GetName() }

func actorRel(rel string, r *ateapi.ObjectRef, info string) relation {
	return relation{rel: rel, kind: kActors, scope: r.GetAtespace(), filter: r.GetName(), id: refID(r), name: refID(r), info: info}
}

func tagRel(rel string, r *ateapi.ObjectRef, info string) relation {
	return relation{rel: rel, kind: kTags, scope: r.GetAtespace(), filter: r.GetName(), id: refID(r), name: refID(r), info: info}
}

func workerRel(name string, ws []*ateapi.Worker) relation {
	r := relation{rel: "worker", kind: kWorkers, filter: name, id: name, name: name}
	switch w := findWorker(ws, name); {
	case ws == nil:
	case w == nil:
		r.info, r.level = "not registered", lvError
	default:
		st := w.GetStatus()
		r.info = fmt.Sprintf("%s  %d/%d actors", explain.WorkerState(st.GetState()), st.GetAllocated().GetActors(), st.GetCapacity().GetActors())
		r.level = workerRow(w, time.Time{}).level
	}
	return r
}

func poolRel(namespace, name string) relation {
	return relation{rel: "pool", kind: kWorkerPools, filter: name, id: namespace + "/" + name, name: namespace + "/" + name}
}

func nodeRel(node string) relation {
	return relation{rel: "node", kind: kWorkers, filter: node, name: node, info: "workers on this node"}
}

// actorRelations: template, source tag, golden tag, worker, pool, node.
// tpl and workers may be nil when they could not be loaded.
func actorRelations(a *ateapi.Actor, tpl *ateapi.ActorTemplate, workers []*ateapi.Worker) []relation {
	var out []relation
	t := relation{rel: "template", kind: kTemplates, scope: a.GetActorTemplate().GetAtespace(),
		filter: a.GetActorTemplate().GetName(), id: refID(a.GetActorTemplate()), name: refID(a.GetActorTemplate())}
	if tpl == nil {
		t.info, t.level = "not loaded", lvWarn
	}
	out = append(out, t)
	if s := a.GetSourceTag(); s.GetName() != "" {
		out = append(out, tagRel("source tag", s, "the actor started from this tag"))
	}
	gs := tpl.GetStatus().GetGoldenSnapshotStatus()
	switch {
	case gs.GetErrorMessage() != "":
		out = append(out, relation{rel: "golden tag", name: "-", info: "golden snapshot failed: " + gs.GetErrorMessage(), level: lvError})
	case gs.GetGoldenTag().GetName() != "":
		out = append(out, tagRel("golden tag", gs.GetGoldenTag(), "the template's golden snapshot"))
	}
	st := a.GetStatus()
	if wa := st.GetWorkerAssignment(); wa.GetWorker().GetName() != "" {
		out = append(out, workerRel(wa.GetWorker().GetName(), workers))
		if wa.GetWorkerPool() != "" {
			out = append(out, poolRel(wa.GetWorkerNamespace(), wa.GetWorkerPool()))
		}
	}
	node := st.GetWorkerAssignment().GetNodeName()
	if node == "" {
		node = st.GetAssignedNode()
	}
	if node != "" {
		out = append(out, nodeRel(node))
	}
	return out
}

// workerRelations: pool, node and the actors assigned to the worker.
func workerRelations(w *ateapi.Worker, as []*ateapi.ActorAssignment, pools []*infrav1.WorkerPool) []relation {
	var out []relation
	if w.GetWorkerPool() != "" {
		p := poolRel(w.GetWorkerNamespace(), w.GetWorkerPool())
		for _, pl := range pools {
			if pl.Namespace == w.GetWorkerNamespace() && pl.Name == w.GetWorkerPool() {
				p.info = fmt.Sprintf("%d/%d ready", pl.Status.ReadyReplicas, pl.Spec.Replicas)
				p.level = poolRow(pl, time.Time{}).level
			}
		}
		out = append(out, p)
	}
	if n := w.GetNodeName(); n != "" {
		out = append(out, nodeRel(n))
	}
	for _, a := range as {
		out = append(out, actorRel("assigned actor", a.GetActor(),
			"cpu "+explain.Millicores(a.GetResources().GetCpuMillicores())+"  mem "+explain.Bytes(a.GetResources().GetMemoryBytes())))
	}
	return out
}

// templateRelations: the golden tag and the actors built from the template.
func templateRelations(t *ateapi.ActorTemplate, actors []*ateapi.Actor) []relation {
	var out []relation
	gs := t.GetStatus().GetGoldenSnapshotStatus()
	switch {
	case gs.GetErrorMessage() != "":
		out = append(out, relation{rel: "golden tag", name: "-", info: "golden snapshot failed: " + gs.GetErrorMessage(), level: lvError})
	case gs.GetGoldenTag().GetName() != "":
		out = append(out, tagRel("golden tag", gs.GetGoldenTag(), ""))
	}
	md := t.GetMetadata()
	for _, a := range actors {
		ref := a.GetActorTemplate()
		if ref.GetAtespace() != md.GetAtespace() || ref.GetName() != md.GetName() {
			continue
		}
		r := actorRel("actor", &ateapi.ObjectRef{Atespace: a.GetMetadata().GetAtespace(), Name: a.GetMetadata().GetName()},
			explain.ActorState(a.GetStatus().GetState()))
		r.level = actorRow(a, time.Now()).level
		out = append(out, r)
	}
	return out
}

func sandboxConfigRel(rel, name, info string) relation {
	return relation{rel: rel, kind: kSandboxCfgs, filter: name, id: name, name: name, info: info}
}

// poolRelations: the SandboxConfig it resolved or is switching to, and the
// workers that registered from the pool.
func poolRelations(p *infrav1.WorkerPool, workers []*ateapi.Worker) []relation {
	var out []relation
	for _, sc := range p.Status.SandboxClasses {
		if sc.ConfigRef != nil {
			out = append(out, sandboxConfigRel("sandbox config", sc.ConfigRef.Name, string(sc.Name)))
		}
		if sc.PendingConfigRef != nil {
			out = append(out, sandboxConfigRel("switching to", sc.PendingConfigRef.Name, string(sc.Name)))
		}
	}
	for _, w := range workers {
		if w.GetWorkerPool() == p.Name && w.GetWorkerNamespace() == p.Namespace {
			out = append(out, workerRel(w.GetMetadata().GetName(), workers))
		}
	}
	return out
}

// sandboxConfigRelations: the pools that use the config or switch to it.
func sandboxConfigRelations(c *infrav1.SandboxConfig, pools []*infrav1.WorkerPool) []relation {
	var out []relation
	for _, p := range pools {
		for _, sc := range p.Status.SandboxClasses {
			rel := ""
			switch {
			case sc.ConfigRef != nil && sc.ConfigRef.Name == c.Name:
				rel = "used by"
			case sc.PendingConfigRef != nil && sc.PendingConfigRef.Name == c.Name:
				rel = "switching pool"
			default:
				continue
			}
			r := poolRel(p.Namespace, p.Name)
			r.rel = rel
			row := poolRow(p, time.Time{})
			r.info, r.level = fmt.Sprintf("%d/%d ready", p.Status.ReadyReplicas, p.Spec.Replicas), row.level
			out = append(out, r)
		}
	}
	return out
}

// relationLines renders the items with the cursor row highlighted.
func relationLines(items []relation, cursor, width int) []string {
	if len(items) == 0 {
		return []string{stDim.Render("no related objects")}
	}
	cols := []string{"RELATION", "KIND", "NAME", "INFO"}
	w := []int{len(cols[0]), len(cols[1]), len(cols[2]), len(cols[3])}
	for _, it := range items {
		w[0] = max(w[0], len(it.rel))
		w[1] = max(w[1], len(it.kind))
		w[2] = max(w[2], min(len(it.name), maxColWidth))
	}
	row := func(c []string) string {
		return fmt.Sprintf(" %-*s  %-*s  %-*s  %s", w[0], c[0], w[1], c[1], w[2], c[2], c[3])
	}
	out := []string{stColHeader.Render(row(cols))}
	for i, it := range items {
		k := string(it.kind)
		if k == "" {
			k = "-"
		}
		line := row([]string{it.rel, k, it.name, it.info})
		if i == cursor {
			out = append(out, stSelected.Render(fit(line, width)))
		} else {
			out = append(out, rowStyles[it.level].Render(line))
		}
	}
	return out
}

func (m *Model) relationsLoaded(msg relationsMsg) {
	p := m.pager
	if p == nil || msg.gen != p.gen {
		return
	}
	p.loading = false
	if msg.err != nil {
		p.items = nil
		p.setLines([]string{stStatusErr.Render(errString(msg.err))}, m.pagerHeight())
		return
	}
	p.items = msg.items
	if p.items == nil {
		p.items = []relation{}
	}
	p.cursor = 0
	m.drawRelations()
}

func (m *Model) drawRelations() {
	p := m.pager
	h := m.pagerHeight()
	p.lines = relationLines(p.items, p.cursor, m.width-4)
	// Row 0 is the column header; keep the cursor's row on screen.
	row := p.cursor + 1
	if row < p.offset+1 {
		p.offset = max(row-1, 0)
	}
	if row >= p.offset+h {
		p.offset = row - h + 1
	}
	p.scroll(0, h)
}

// keyRelations moves the cursor and jumps; it reports whether it
// consumed the key.
func (m *Model) keyRelations(key string) (bool, tea.Cmd) {
	p := m.pager
	n := len(p.items)
	move := func(d int) {
		if n > 0 {
			p.cursor = min(max(p.cursor+d, 0), n-1)
			m.drawRelations()
		}
	}
	h := m.pagerHeight()
	switch key {
	case "j", "down":
		move(1)
	case "k", "up":
		move(-1)
	case "g", "home":
		move(-n)
	case "G", "end":
		move(n)
	case "ctrl+f", "pgdown":
		move(h)
	case "ctrl+b", "pgup":
		move(-h)
	case "enter":
		if p.cursor < n {
			return true, m.jump(p.items[p.cursor])
		}
	default:
		return false, nil
	}
	return true, nil
}

// jump shows a relation's object in its table, with the cursor on it.
func (m *Model) jump(r relation) tea.Cmd {
	if r.kind == "" {
		m.setStatus(strings.TrimSpace(r.rel+" "+r.info), r.level >= lvError)
		return nil
	}
	if lookupKind(string(r.kind)).scoped {
		m.atespace = r.scope
	}
	key := m.keyFor(r.kind)
	m.want = wantRow{key: key, id: r.id}
	return m.gotoKind(r.kind, r.filter)
}

// wantRow is a row a jump should land the cursor on once it is listed.
type wantRow struct {
	key viewKey
	id  string
}
