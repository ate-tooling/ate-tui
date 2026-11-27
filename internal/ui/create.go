package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Creating opens $EDITOR on a manifest, the same protojson-shaped YAML
// `kubectl ate create actor-template -f` reads and `y` shows, or for an ate
// CRD the YAML kubectl reads. A bad manifest reopens the editor with the
// error on top; an unchanged or empty one cancels. The parsed object then
// goes through the usual confirm dialog. Editing a CRD works the same way
// (see kubeedit.go).

// createAction is the write a confirmed manifest runs.
var createAction = &action{name: "create", past: "created", pending: "Creating",
	run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
		switch o := obj.(type) {
		case *ateapi.Atespace:
			return asAny(b.CreateAtespace(ctx, o))
		case *ateapi.ActorTemplate:
			return asAny(b.CreateActorTemplate(ctx, o))
		case *ateapi.Actor:
			return asAny(b.CreateActor(ctx, o))
		case *ateapi.Tag:
			return asAny(b.CreateTag(ctx, o))
		case metav1.Object:
			return b.CreateObject(ctx, o)
		}
		return nil, fmt.Errorf("cannot create %T", obj)
	}}

func creatable(k kind) bool {
	return k == kAtespaces || k == kTemplates || k == kActors || k == kTags || kubeKind(k)
}

type (
	// editMsg asks to open the editor on path.
	editMsg struct {
		key      viewKey
		act      *action // createAction or editAction
		path     string
		original string // the manifest as offered, to spot "no changes"
		// orig is the object an edit started from.
		orig any
		// retried is set once the server failed this manifest: original is
		// then the user's text, which a cancel keeps on disk.
		retried bool
	}
	editedMsg struct {
		editMsg
		err error
	}
	// manifestMsg is a parsed manifest, or the reason it was not.
	manifestMsg struct {
		editMsg
		obj       any
		id        string
		text      string   // the manifest as saved, to edit again if the server refuses it
		diff      []string // what an edit changes
		cancelled bool
		emptied   bool // the user emptied the file to cancel
		// kept is where a manifest the server failed was saved; note is
		// the status to show with it.
		kept string
		note string
		err  error
	}
)

// editorHeader starts every manifest; lines starting with # are comments.
const editorHeader = "# Save and quit to create. Quit without saving, or empty the file, to cancel.\n"

// errorPrefix marks the error comment a failed parse puts on top.
const errorPrefix = "# error: "

// startCreate opens the editor on a starter manifest for the view's kind,
// or with clone on a copy of the selected object.
func (m *Model) startCreate(clone bool) tea.Cmd {
	k := m.cur.kind
	switch {
	case !creatable(k):
		m.setStatus("cannot create "+string(k)+" here; creating works on atespaces, templates, actors, tags, workerpools, sandboxconfigs and csidriverconfigs", true)
		return nil
	case m.backend == nil:
		m.setStatus("not connected", true)
		return nil
	case !m.writable():
		m.setStatus(m.readOnlyHint(), true)
		return nil
	}
	var body string
	if clone {
		r := m.selected()
		if r == nil {
			return nil
		}
		b, err := cloneManifest(r.obj)
		if err != nil {
			m.setStatus("clone: "+err.Error(), true)
			return nil
		}
		body = "# A copy of " + r.id + " without its server-set fields. Rename it.\n" + b
	} else {
		body = m.starterManifest(k)
	}
	return writeManifest(editMsg{key: m.cur, act: createAction}, editorHeader+body)
}

// writeManifest puts content in a temp file and asks to edit it. Saving
// msg.original, or content when that is empty, unchanged cancels.
func writeManifest(msg editMsg, content string) tea.Cmd {
	if msg.original == "" {
		msg.original = content
	}
	return func() tea.Msg {
		path, err := writeTemp(content)
		if err != nil {
			return manifestMsg{editMsg: editMsg{act: msg.act}, err: err}
		}
		msg.path = path
		return msg
	}
}

// writeTemp saves content in a new temp file, and removes the file again
// if any step fails, so no partial manifest stays behind.
func writeTemp(content string) (string, error) {
	f, err := os.CreateTemp("", "ate-tui-*.yaml")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// editor is $VISUAL, then $EDITOR, then vi, split into words so
// "code --wait" works.
func editor() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(env)); len(f) > 0 {
			return f
		}
	}
	return []string{"vi"}
}

// openEditor hands the terminal to the editor; Bubble Tea takes it back
// when the editor exits. Tests replace m.edit.
func (m *Model) openEditor(msg editMsg) tea.Cmd {
	if m.edit != nil {
		return m.edit(msg.path, func(err error) tea.Msg { return editedMsg{msg, err} })
	}
	args := editor()
	c := exec.Command(args[0], append(args[1:], msg.path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return editedMsg{msg, err} })
}

// edited reads and parses the saved manifest off the UI goroutine.
func (m *Model) edited(msg editedMsg) tea.Cmd {
	if msg.err != nil {
		os.Remove(msg.path)
		m.setStatus("editor: "+msg.err.Error(), true)
		return nil
	}
	k := msg.key.kind
	return func() tea.Msg {
		out := manifestMsg{editMsg: msg.editMsg}
		b, err := os.ReadFile(msg.path)
		if err != nil {
			out.err = err
			return out
		}
		text := stripErrors(string(b))
		if strings.TrimSpace(uncomment(text)) == "" {
			out.cancelled, out.emptied = true, true
			return out
		}
		if text == stripErrors(msg.original) {
			out.cancelled = true
			return out
		}
		out.text = text
		switch {
		case msg.act == editAction:
			out.obj, out.id, out.diff, out.err = parseEdit(msg.orig, []byte(text))
			if out.err == nil && len(out.diff) == 0 {
				out.cancelled = true
				return out
			}
		case kubeKind(k):
			out.obj, out.id, out.err = parseKube(k, []byte(text))
		default:
			out.obj, out.id, out.err = parseManifest(k, []byte(text))
		}
		if out.err != nil {
			// Put the error on top and edit again.
			again := errorComment(out.err.Error()) + text
			if werr := os.WriteFile(msg.path, []byte(again), 0o600); werr != nil {
				out.err = werr
			}
		}
		return out
	}
}

func (m *Model) manifestParsed(msg manifestMsg) tea.Cmd {
	name := msg.act.name
	switch {
	case msg.kept != "":
		m.setStatus(msg.note+"; your manifest is kept at "+msg.kept, true)
		return nil
	case msg.note != "":
		m.setStatus(msg.note, true)
		return nil
	case msg.cancelled:
		kept := ""
		if msg.retried && !msg.emptied {
			// The file holds the user's text the server failed; keep it.
			kept = "; your manifest is kept at " + msg.path
		} else {
			os.Remove(msg.path)
		}
		if name == "edit" {
			m.setStatus("no changes; edit cancelled"+kept, false)
		} else {
			m.setStatus(name+" cancelled"+kept, false)
		}
		return nil
	case msg.err != nil && msg.path == "":
		m.setStatus(name+": "+msg.err.Error(), true)
		return nil
	case msg.err != nil:
		m.setStatus("manifest: "+msg.err.Error()+" (editing again)", true)
		return m.openEditor(msg.editMsg)
	}
	if msg.key != m.cur || m.backend == nil {
		m.setStatus("the view changed while editing; "+name+" cancelled; your manifest is kept at "+msg.path, true)
		return nil
	}
	os.Remove(msg.path)
	retry := msg.editMsg
	retry.path, retry.original = "", msg.text
	m.confirm = &confirm{act: msg.act, key: msg.key, targets: []target{{id: msg.id, name: msg.id, obj: msg.obj}},
		diff: msg.diff, retry: &retry, warnings: m.manifestWarnings(msg.key, msg.obj)}
	m.mode, m.input = modeConfirm, ""
	return nil
}

// refused reports whether err is the server rejecting a manifest, which
// is worth editing again rather than retrying.
func refused(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.AlreadyExists, codes.FailedPrecondition:
		return true
	}
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsAlreadyExists(err)
}

// manifestFailed keeps the user's text after the server failed a create or
// an edit. A refusal or a Conflict reopens the editor on it with the error
// on top, as kubectl edit does; any other failure (a timeout, Forbidden,
// NotFound, Unavailable), or one after the view changed, saves it to a
// temp file and says where.
func (m *Model) manifestFailed(bt *actionBatch, err error) tea.Cmd {
	retry := *bt.retry
	retry.retried = true
	summary := batchSummary(bt)
	if bt.key == m.cur && m.backend != nil && (refused(err) || apierrors.IsConflict(err)) {
		m.setStatus(summary+" (editing again)", true)
		if apierrors.IsConflict(err) && retry.act == editAction {
			return m.rebaseEdit(retry, err)
		}
		return editAgain(retry, err, "")
	}
	m.setStatus(summary, true)
	text := errorComment(errString(err)) + retry.original
	return func() tea.Msg {
		path, werr := writeTemp(text)
		if werr != nil {
			return manifestMsg{editMsg: editMsg{act: retry.act}, note: summary + "; your manifest could not be kept: " + werr.Error()}
		}
		return manifestMsg{editMsg: editMsg{act: retry.act}, kept: path, note: summary}
	}
}

// errorComment is s as the error comment lines a manifest starts with.
func errorComment(s string) string {
	return errorPrefix + strings.ReplaceAll(s, "\n", "\n"+errorPrefix) + "\n"
}

// editAgain reopens the editor on the user's text with the server's error,
// and note if any, on top. Saving it unchanged cancels, as kubectl edit
// does, and keeps the text on disk.
func editAgain(retry editMsg, err error, note string) tea.Cmd {
	head := errorComment(errString(err))
	if note != "" {
		head += errorComment(note)
	}
	return writeManifest(retry, head+retry.original)
}

// rebaseEdit handles an edit that lost a race: it reads the object again,
// moves the user's text onto the latest resourceVersion, and reopens the
// editor. Saving it with a change applies the text over the latest
// version, and the confirm dialog shows the diff against that version, so
// a change made meanwhile that the text would undo is visible first.
func (m *Model) rebaseEdit(retry editMsg, err error) tea.Cmd {
	o, ok := retry.orig.(metav1.Object)
	kd := lookupKind(string(retry.key.kind))
	if !ok || kd == nil || kd.fetch == nil {
		return editAgain(retry, err, "")
	}
	b, id, scope := m.backend, objID(o), retry.key.scope
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		rows, ferr := kd.fetch(ctx, b, scope, time.Now())
		if ferr != nil {
			return editAgain(retry, err, "could not read the latest version: "+errString(ferr)+"; saving retries against the version you edited")()
		}
		for _, r := range rows {
			latest, ok := r.obj.(metav1.Object)
			if r.id != id || !ok {
				continue
			}
			retry.orig = r.obj
			text, ok := setResourceVersion(retry.original, latest.GetResourceVersion())
			retry.original = text
			if !ok {
				return editAgain(retry, err, "it changed since you read it: set metadata.resourceVersion to "+strconv.Quote(latest.GetResourceVersion())+" (the latest), or remove it, then save to apply your text over that version; quit to cancel")()
			}
			return editAgain(retry, err, "it changed since you read it: your text below now targets the latest version. Save it with any change to apply it over that version (the confirm dialog diffs against it), or quit to cancel")()
		}
		return editAgain(retry, err, id+" no longer exists")()
	}
}

// resourceVersionLine is metadata's resourceVersion in block style, at any
// indent; managedFields, the only other place it can appear, is removed
// before editing.
var resourceVersionLine = regexp.MustCompile(`(?m)^([ \t]+)resourceVersion:.*$`)

// setResourceVersion points the metadata of a manifest at rv. Without a
// resourceVersion line parseEdit takes the edited object's, which is rv. ok
// is false when the text still names another version (a layout the line
// match does not know, such as flow style).
func setResourceVersion(text, rv string) (string, bool) {
	if loc := resourceVersionLine.FindStringSubmatchIndex(text); loc != nil {
		text = text[:loc[0]] + text[loc[2]:loc[3]] + "resourceVersion: " + strconv.Quote(rv) + text[loc[1]:]
	}
	var obj struct {
		Metadata struct {
			ResourceVersion *string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(text), &obj); err != nil {
		// parseEdit reports the YAML error itself.
		return text, true
	}
	got := obj.Metadata.ResourceVersion
	return text, got == nil || *got == rv
}

// parseManifest reads one protojson-shaped YAML document strictly, so a
// typo is an error rather than a dropped field, and checks what ateapi
// needs before anything is sent.
func parseManifest(k kind, data []byte) (any, string, error) {
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, "", fmt.Errorf("invalid YAML: %w", err)
	}
	var obj proto.Message
	switch k {
	case kAtespaces:
		obj = &ateapi.Atespace{}
	case kTemplates:
		obj = &ateapi.ActorTemplate{}
	case kActors:
		obj = &ateapi.Actor{}
	case kTags:
		obj = &ateapi.Tag{}
	default:
		return nil, "", fmt.Errorf("cannot create %s", k)
	}
	if err := protojson.Unmarshal(j, obj); err != nil {
		return nil, "", err
	}
	md := obj.(interface {
		GetMetadata() *ateapi.ResourceMetadata
	}).GetMetadata()
	if md.GetName() == "" {
		return nil, "", fmt.Errorf("metadata.name is required")
	}
	if k == kAtespaces {
		if md.GetAtespace() != "" {
			return nil, "", fmt.Errorf("an atespace has no metadata.atespace")
		}
		return obj, md.GetName(), nil
	}
	if md.GetAtespace() == "" {
		return nil, "", fmt.Errorf("metadata.atespace is required")
	}
	switch o := obj.(type) {
	case *ateapi.ActorTemplate:
		if len(o.GetContainers()) == 0 {
			return nil, "", fmt.Errorf("containers: at least one container is required")
		}
		for i, c := range o.GetContainers() {
			if c.GetName() == "" || c.GetImage() == "" {
				return nil, "", fmt.Errorf("containers[%d]: name and image are required", i)
			}
		}
	case *ateapi.Actor:
		if o.GetActorTemplate().GetName() == "" {
			return nil, "", fmt.Errorf("actor_template.name is required")
		}
		if o.ActorTemplate.Atespace == "" {
			o.ActorTemplate.Atespace = md.GetAtespace()
		}
	case *ateapi.Tag:
		if o.GetSourceActor().GetName() == "" {
			return nil, "", fmt.Errorf("source_actor.name is required")
		}
		if o.SourceActor.Atespace == "" {
			o.SourceActor.Atespace = md.GetAtespace()
		}
	}
	return obj, md.GetAtespace() + "/" + md.GetName(), nil
}

// stripErrors drops the error comments a failed parse added on top.
func stripErrors(s string) string {
	for strings.HasPrefix(s, errorPrefix) {
		_, s, _ = strings.Cut(s, "\n")
	}
	return s
}

// uncomment drops comment lines, to tell an emptied file from a real one.
func uncomment(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// cloneManifest is obj as a manifest, without the fields the server sets,
// and with "-copy" on its name.
func cloneManifest(obj any) (string, error) {
	if o, ok := obj.(metav1.Object); ok {
		return cloneKube(o)
	}
	var c proto.Message
	switch o := obj.(type) {
	case *ateapi.Atespace:
		c = proto.Clone(o)
	case *ateapi.ActorTemplate:
		t := proto.Clone(o).(*ateapi.ActorTemplate)
		t.Status = nil
		c = t
	case *ateapi.Actor:
		a := proto.Clone(o).(*ateapi.Actor)
		a.Status = nil
		c = a
	case *ateapi.Tag:
		t := proto.Clone(o).(*ateapi.Tag)
		t.Status = nil
		c = t
	default:
		return "", fmt.Errorf("cannot clone %T", obj)
	}
	md := c.(interface {
		GetMetadata() *ateapi.ResourceMetadata
	}).GetMetadata()
	if md == nil {
		return "", fmt.Errorf("object has no metadata")
	}
	*md = ateapi.ResourceMetadata{Atespace: md.GetAtespace(), Name: md.GetName() + "-copy"}
	j, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(c)
	if err != nil {
		return "", err
	}
	y, err := yaml.JSONToYAML(j)
	return string(y), err
}

// starterManifest is a commented skeleton, filled from the current
// atespace and the selected row where it can be.
func (m *Model) starterManifest(k kind) string {
	as := m.atespace
	if as == "" {
		as = "default"
	}
	tpl, actor := "", ""
	if r := m.selected(); r != nil {
		switch o := r.obj.(type) {
		case *ateapi.Actor:
			as = o.GetMetadata().GetAtespace()
			tpl, actor = o.GetActorTemplate().GetName(), o.GetMetadata().GetName()
		case *ateapi.ActorTemplate:
			as, tpl = o.GetMetadata().GetAtespace(), o.GetMetadata().GetName()
		}
	}
	if kubeKind(k) {
		return m.starterKube(k)
	}
	switch k {
	case kAtespaces:
		return `# An atespace.
metadata:
  name: ""
`
	case kTemplates:
		return fmt.Sprintf(`# An actor template. Templates are immutable: delete and recreate to change one.
# For every field, press y on a template: this file takes the same shape.
metadata:
  atespace: %s
  name: ""
containers:
  - name: agent
    image: ""
    # args: ["--port", "8080"]
resources:
  limits:
    cpu_millicores: 1000
    memory_bytes: 2147483648
`, as)
	case kActors:
		return fmt.Sprintf(`# An actor, created from a template.
metadata:
  atespace: %s
  name: ""
actor_template:
  name: %q   # in the actor's atespace
`, as, tpl)
	case kTags:
		return fmt.Sprintf(`# A tag on an actor's snapshot.
metadata:
  atespace: %s
  name: ""
scope: TAG_SCOPE_ATESPACE   # or TAG_SCOPE_PUBLISHED
source_actor:
  name: %q   # in the tag's atespace
`, as, actor)
	}
	return ""
}

// manifestWarnings flags what a create or edit would do that is likely a
// mistake. A SandboxConfig marked as its class's default, when another
// config of the class already is, makes two: easy to do by starting from
// `kubectl get -o yaml` of the current default.
func (m *Model) manifestWarnings(k viewKey, obj any) []string {
	c, ok := obj.(*infrav1.SandboxConfig)
	if !ok || c.Annotations[infrav1.SandboxConfigClassDefaultAnnotation] != "true" {
		return nil
	}
	d := m.data[k]
	if d == nil {
		return nil
	}
	var others []string
	for _, r := range d.rows {
		o, ok := r.obj.(*infrav1.SandboxConfig)
		if !ok || o.Name == c.Name || o.DeletionTimestamp != nil || o.Spec.SandboxClass != c.Spec.SandboxClass ||
			o.Annotations[infrav1.SandboxConfigClassDefaultAnnotation] != "true" {
			continue
		}
		others = append(others, o.Name)
	}
	if len(others) == 0 {
		return nil
	}
	sort.Strings(others)
	return []string{fmt.Sprintf("%s is already the default for class %s; this makes %d defaults. Remove the %s annotation unless that is intended.",
		strings.Join(others, ", "), c.Spec.SandboxClass, len(others)+1, infrav1.SandboxConfigClassDefaultAnnotation)}
}
