package ui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// action is one write. Every action asks for confirmation and runs off the
// UI goroutine, in parallel over the marked rows.
type action struct {
	name string // "suspend", as typed in the journal
	past string // "suspended", for the status line
	key  string
	kind kind
	// typed asks the user to type the object name (or "yes") to confirm.
	typed bool
	// prompt names the number the dialog asks for ("replicas"), if any.
	prompt string
	// pending is the state shown when the RPC returns no object.
	pending  string
	stateCol int
	run      func(ctx context.Context, b Backend, obj any, in actionInput) (any, error)
}

// actionInput is what the confirm dialog adds to an action.
type actionInput struct {
	sync   bool  // suspend: synchronous durability
	number int32 // the prompt's value
}

func actorRef(obj any) *ateapi.ObjectRef {
	md := obj.(*ateapi.Actor).GetMetadata()
	return &ateapi.ObjectRef{Atespace: md.GetAtespace(), Name: md.GetName()}
}

func workerName(obj any) string { return obj.(*ateapi.Worker).GetMetadata().GetName() }

// asAny keeps a typed nil from becoming a non-nil interface.
func asAny[T any](v *T, err error) (any, error) {
	if v == nil {
		return nil, err
	}
	return v, err
}

var actions = []*action{
	{name: "suspend", past: "suspended", key: "s", kind: kActors, pending: "Suspending", stateCol: 2,
		run: func(ctx context.Context, b Backend, obj any, in actionInput) (any, error) {
			mode := ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS
			if in.sync {
				mode = ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS
			}
			return asAny(b.SuspendActor(ctx, actorRef(obj), mode))
		}},
	{name: "resume", past: "resumed", key: "R", kind: kActors, pending: "Resuming", stateCol: 2,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			return asAny(b.ResumeActor(ctx, actorRef(obj)))
		}},
	{name: "revert", past: "reverted", key: "v", kind: kActors, pending: "Reverting", stateCol: 2,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			return asAny(b.RevertActor(ctx, actorRef(obj)))
		}},
	{name: "delete", past: "deleted", key: "ctrl+d", kind: kActors, typed: true, pending: "Deleting", stateCol: 2,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			a, err := b.DeleteActor(ctx, actorRef(obj))
			if a != nil {
				// Show the delete in progress whatever the returned record says.
				if a.Status == nil {
					a.Status = &ateapi.ActorStatus{}
				}
				a.Status.State = ateapi.ActorState_ACTOR_STATE_DELETING
			}
			return asAny(a, err)
		}},
	{name: "drain", past: "drained", key: "D", kind: kWorkers, pending: "Draining", stateCol: 1,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			return asAny(b.DrainWorker(ctx, workerName(obj)))
		}},
	{name: "mark available", past: "marked available", key: "A", kind: kWorkers, pending: "Active", stateCol: 1,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			return asAny(b.MarkWorkerAvailable(ctx, workerName(obj)))
		}},
	{name: "mark unavailable", past: "marked unavailable", key: "U", kind: kWorkers, pending: "Unavailable", stateCol: 1,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			return asAny(b.MarkWorkerUnavailable(ctx, workerName(obj)))
		}},
	{name: "scale", past: "scaled", key: "z", kind: kWorkerPools, prompt: "replicas", pending: "Scaling", stateCol: 7,
		run: func(ctx context.Context, b Backend, obj any, in actionInput) (any, error) {
			p := obj.(*infrav1.WorkerPool)
			return asAny(b.ScaleWorkerPool(ctx, p.Namespace, p.Name, in.number))
		}},
	kubeDelete(kWorkerPools, 7), kubeDelete(kSandboxCfgs, 6), kubeDelete(kCSIDrivers, 4),
}

// kubeDelete deletes a CRD. The row stays, Terminating, until the re-poll
// finds it gone; a SandboxConfig a pool still uses stays until it is released.
func kubeDelete(k kind, stateCol int) *action {
	return &action{name: "delete", past: "deleted", key: "ctrl+d", kind: k, typed: true, pending: "Terminating", stateCol: stateCol,
		run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
			o := obj.(metav1.Object)
			if err := b.DeleteObject(ctx, o); err != nil {
				return nil, err
			}
			return terminating(obj), nil
		}}
}

// terminating is a copy of obj marked as being deleted, for the row.
func terminating(obj any) any {
	now := metav1.Now()
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		c := o.DeepCopy()
		c.DeletionTimestamp = &now
		return c
	case *infrav1.SandboxConfig:
		c := o.DeepCopy()
		c.DeletionTimestamp = &now
		return c
	case *infrav1.CSIDriverConfig:
		c := o.DeepCopy()
		c.DeletionTimestamp = &now
		return c
	}
	return nil
}

func lookupAction(k kind, key string) *action {
	for _, a := range actions {
		if a.kind == k && a.key == key {
			return a
		}
	}
	return nil
}

// target is one object an action applies to.
type target struct {
	id   string
	name string // what a typed confirm asks for
	obj  any
}

// confirm is the open confirmation dialog.
type confirm struct {
	act     *action
	key     viewKey
	targets []target
	sync    bool
	refused bool // the typed text did not match on the last ⏎
	// diff is what an edit changes, shown in the dialog.
	diff []string
	// retry reopens the editor if the server refuses a create or an edit.
	retry *editMsg
	// warnings are what the manifest does that is likely a mistake, such
	// as a second default for a sandbox class.
	warnings []string
}

// want is the text a typed confirm needs.
func (c *confirm) want() string {
	if len(c.targets) == 1 {
		return c.targets[0].name
	}
	return "yes"
}

type (
	actionResultMsg struct {
		batch int
		id    string
		obj   any
		// target is the row's object the action ran on.
		target any
		err    error
	}
	repollMsg   struct{ key viewKey }
	journalMsg  struct{ err error }
	actionBatch struct {
		act    *action
		key    viewKey
		total  int
		done   int
		failed int
		first  error
		single string
		in     actionInput
		retry  *editMsg
	}
)

const (
	actionTimeout = 30 * time.Second
	// A synchronous suspend waits for the snapshot upload.
	syncSuspendTimeout = 5 * time.Minute
	actionParallelism  = 8
)

// writable reports whether writes are enabled for the connected context.
func (m *Model) writable() bool {
	return m.opts.Write || (m.info.Context != "" && slices.Contains(m.opts.WriteContexts, m.info.Context))
}

func (m *Model) readOnlyHint() string {
	ctx := m.info.Context
	if ctx == "" {
		ctx = "<context>"
	}
	cfg := m.opts.ConfigPath
	if cfg == "" {
		cfg = "~/.config/ate-tui/config.yaml"
	}
	return fmt.Sprintf("read-only: restart with --write, or add %q to writeContexts in %s", ctx, cfg)
}

// toggleMark marks or unmarks the current row.
func (m *Model) toggleMark() {
	r := m.selected()
	if r == nil {
		return
	}
	t := m.table()
	t.marksGone = false
	if t.marked[r.id] {
		delete(t.marked, r.id)
	} else {
		t.marked[r.id] = true
	}
}

// pruneMarks drops the marks of rows the view's data no longer lists, so
// the title counts only live marks and an object recreated under the same
// id does not rejoin the selection.
func (m *Model) pruneMarks(k viewKey) {
	t, d := m.tableFor(k), m.data[k]
	if d == nil || len(t.marked) == 0 {
		return
	}
	live := make(map[string]bool, len(d.rows))
	for i := range d.rows {
		live[d.rows[i].id] = true
	}
	for id := range t.marked {
		if !live[id] {
			delete(t.marked, id)
		}
	}
	if len(t.marked) == 0 {
		t.marksGone = true
	}
}

// targets are the marked rows that still exist, or the current row when
// nothing is marked. When every marked row is gone it returns none rather
// than the cursor row the user never marked, and says so.
func (m *Model) targets() []target {
	t := m.table()
	m.pruneMarks(m.cur)
	hadMarks := t.marksGone
	t.marksGone = false
	d := m.data[m.cur]
	mk := func(r *row) target {
		name := r.id
		if md, ok := r.obj.(interface {
			GetMetadata() *ateapi.ResourceMetadata
		}); ok {
			name = md.GetMetadata().GetName()
		} else if o, ok := r.obj.(metav1.Object); ok {
			name = o.GetName()
		}
		return target{id: r.id, name: name, obj: r.obj}
	}
	var out []target
	if d != nil && len(t.marked) > 0 {
		for i := range d.rows {
			if t.marked[d.rows[i].id] {
				out = append(out, mk(&d.rows[i]))
			}
		}
	}
	if len(out) == 0 && hadMarks {
		m.setStatus("the marked rows no longer exist; marks cleared, nothing was changed", true)
		return nil
	}
	if len(out) == 0 {
		if r := m.selected(); r != nil {
			out = append(out, mk(r))
		}
	}
	return out
}

// startAction opens the confirm dialog, or explains why it cannot.
func (m *Model) startAction(a *action) tea.Cmd {
	switch {
	case m.backend == nil:
		m.setStatus("not connected", true)
		return nil
	case !m.writable():
		m.setStatus(m.readOnlyHint(), true)
		return nil
	}
	ts := m.targets()
	if len(ts) == 0 {
		return nil
	}
	m.confirm = &confirm{act: a, key: m.cur, targets: ts}
	m.mode, m.input = modeConfirm, ""
	if a.prompt != "" && len(ts) == 1 {
		if p, ok := ts[0].obj.(*infrav1.WorkerPool); ok {
			m.input = fmt.Sprint(p.Spec.Replicas)
		}
	}
	return nil
}

func (m *Model) keyConfirm(msg tea.KeyMsg) tea.Cmd {
	c := m.confirm
	cancel := func() tea.Cmd {
		m.confirm, m.mode = nil, modeTable
		m.setStatus(c.act.name+" cancelled", false)
		return nil
	}
	if c.act.prompt != "" {
		switch msg.Type {
		case tea.KeyEsc:
			return cancel()
		case tea.KeyEnter:
			n, err := strconv.ParseInt(strings.TrimSpace(m.input), 10, 32)
			if err != nil || n < 0 {
				c.refused = true
				return nil
			}
			return m.runAction(actionInput{number: int32(n)})
		}
		c.refused = false
		m.editInput(msg)
		return nil
	}
	if c.act.typed {
		switch msg.Type {
		case tea.KeyEsc:
			return cancel()
		case tea.KeyEnter:
			if strings.TrimSpace(m.input) != c.want() {
				c.refused = true
				return nil
			}
			return m.runAction(actionInput{})
		}
		c.refused = false
		m.editInput(msg)
		return nil
	}
	switch msg.String() {
	case "y", "Y", "enter":
		return m.runAction(actionInput{sync: c.sync})
	case "n", "N", "esc", "q":
		return cancel()
	case "tab", "m":
		if c.act.name == "suspend" {
			c.sync = !c.sync
		}
	}
	return nil
}

// runAction starts the confirmed action on every target in parallel.
func (m *Model) runAction(in actionInput) tea.Cmd {
	c := m.confirm
	m.confirm, m.mode = nil, modeTable
	clear(m.tableFor(c.key).marked)
	m.gen++
	batch := m.gen
	m.batches[batch] = &actionBatch{act: c.act, key: c.key, total: len(c.targets), in: in, retry: c.retry}
	if len(c.targets) == 1 {
		m.batches[batch].single = c.targets[0].id
	}
	what := fmt.Sprint(len(c.targets))
	if len(c.targets) == 1 {
		what = c.targets[0].id
	}
	m.setStatus(c.act.name+" "+what+"…", false)

	timeout := actionTimeout
	if in.sync {
		timeout = syncSuspendTimeout
	}
	sem := make(chan struct{}, actionParallelism)
	b, act := m.backend, c.act
	cmds := make([]tea.Cmd, 0, len(c.targets))
	for _, t := range c.targets {
		cmds = append(cmds, func() tea.Msg {
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			obj, err := act.run(ctx, b, t.obj, in)
			return actionResultMsg{batch: batch, id: t.id, obj: obj, target: t.obj, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// actionResult records one object's outcome: it updates the row at once,
// journals it, and reports the batch when the last result is in.
func (m *Model) actionResult(msg actionResultMsg) tea.Cmd {
	bt := m.batches[msg.batch]
	if bt == nil {
		return nil
	}
	bt.done++
	var cmds []tea.Cmd
	if msg.err != nil {
		bt.failed++
		if bt.first == nil {
			bt.first = msg.err
		}
	} else {
		m.optimistic(bt, msg.id, msg.obj)
		if bt.act == createAction && bt.key == m.cur {
			// Land on the new row once the re-poll lists it.
			m.want = wantRow{key: bt.key, id: msg.id}
		}
	}
	cmds = append(cmds, m.journal(bt, msg.id, msg.target, msg.err))
	if bt.done < bt.total {
		return tea.Batch(cmds...)
	}
	delete(m.batches, msg.batch)
	m.setStatus(batchSummary(bt), bt.failed > 0)
	if bt.retry != nil && msg.err != nil {
		// Never drop the user's text: edit it again, or keep it on disk.
		return tea.Batch(append(cmds, m.manifestFailed(bt, msg.err))...)
	}
	if bt.failed < bt.total {
		key := bt.key
		cmds = append(cmds, tea.Tick(m.repoll, func(time.Time) tea.Msg { return repollMsg{key} }))
	}
	return tea.Batch(cmds...)
}

func batchSummary(bt *actionBatch) string {
	ok := bt.total - bt.failed
	switch {
	case bt.single != "" && bt.failed == 0:
		return bt.act.past + " " + bt.single
	case bt.single != "":
		return bt.act.name + " " + bt.single + ": " + actionErr(bt.first)
	case bt.failed == 0:
		return fmt.Sprintf("%s %d", bt.act.past, ok)
	}
	return fmt.Sprintf("%s %d, failed %d: %s", bt.act.past, ok, bt.failed, actionErr(bt.first))
}

// actionErr is errString plus a hint for the errors a user can act on.
func actionErr(err error) string {
	s := errString(err)
	switch status.Code(err) {
	case codes.PermissionDenied:
		s += " (the ate-client principal lacks this permission; pass --token-file with a token that has it)"
	case codes.Unauthenticated:
		s += " (the token was rejected; check --token-file or the ate-client ServiceAccount)"
	}
	switch {
	case apierrors.IsForbidden(err):
		s += " (your kubeconfig user lacks this RBAC permission)"
	case apierrors.IsConflict(err):
		s += " (it changed since it was read)"
	}
	return s
}

// optimistic shows the new state in the row at once. The re-poll that
// follows confirms it.
func (m *Model) optimistic(bt *actionBatch, id string, obj any) {
	d := m.data[bt.key]
	if d == nil {
		return
	}
	i := slices.IndexFunc(d.rows, func(r row) bool { return r.id == id })
	if i < 0 {
		return
	}
	now := time.Now()
	switch o := obj.(type) {
	case *ateapi.Actor:
		d.rows[i] = actorRow(o, now)
	case *ateapi.Worker:
		d.rows[i] = workerRow(o, now)
	case metav1.Object:
		r, _ := m.kubeRow(o, now)
		d.rows[i] = r
	default:
		old := d.rows[i]
		cells := slices.Clone(old.cells)
		cells[bt.act.stateCol] = bt.act.pending
		d.rows[i] = newRow(old.id, old.obj, lvProgress, cells, old.sortBy)
	}
	// A list that started before the write would bring the old state back;
	// drop it.
	if d.loading {
		d.loading = false
		m.gen++
		d.gen = m.gen
	}
	if bt.key == m.cur {
		m.rebuild()
	}
}
