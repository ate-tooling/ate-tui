package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"
)

// Creating opens $EDITOR on a manifest, the same protojson-shaped YAML
// `kubectl ate create actor-template -f` reads and `y` shows. A bad manifest
// reopens the editor with the error on top; an unchanged or empty one
// cancels. The parsed object then goes through the usual confirm dialog.

// createAction is the write a confirmed manifest runs.
var createAction = &action{name: "create", past: "created", pending: "Creating",
	run: func(ctx context.Context, b Backend, obj any, _ bool) (any, error) {
		switch o := obj.(type) {
		case *ateapi.Atespace:
			return asAny(b.CreateAtespace(ctx, o))
		case *ateapi.ActorTemplate:
			return asAny(b.CreateActorTemplate(ctx, o))
		case *ateapi.Actor:
			return asAny(b.CreateActor(ctx, o))
		case *ateapi.Tag:
			return asAny(b.CreateTag(ctx, o))
		}
		return nil, fmt.Errorf("cannot create %T", obj)
	}}

func creatable(k kind) bool {
	return k == kAtespaces || k == kTemplates || k == kActors || k == kTags
}

type (
	// editMsg asks to open the editor on path.
	editMsg struct {
		key      viewKey
		path     string
		original string // the manifest as offered, to spot "no changes"
	}
	editedMsg struct {
		editMsg
		err error
	}
	// manifestMsg is a parsed manifest, or the reason it was not.
	manifestMsg struct {
		editMsg
		obj       proto.Message
		id        string
		cancelled bool
		err       error
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
		m.setStatus("cannot create "+string(k)+" here; creating works on atespaces, templates, actors and tags", true)
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
	content := editorHeader + body
	key := m.cur
	return func() tea.Msg {
		f, err := os.CreateTemp("", "ate-tui-*.yaml")
		if err != nil {
			return manifestMsg{err: err}
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return manifestMsg{err: err}
		}
		return editMsg{key: key, path: f.Name(), original: content}
	}
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
		if text == stripErrors(msg.original) || strings.TrimSpace(uncomment(text)) == "" {
			out.cancelled = true
			return out
		}
		out.obj, out.id, out.err = parseManifest(k, []byte(text))
		if out.err != nil {
			// Put the error on top and edit again.
			again := errorPrefix + strings.ReplaceAll(out.err.Error(), "\n", "\n"+errorPrefix) + "\n" + text
			if werr := os.WriteFile(msg.path, []byte(again), 0o600); werr != nil {
				out.err = werr
			}
		}
		return out
	}
}

func (m *Model) manifestParsed(msg manifestMsg) tea.Cmd {
	switch {
	case msg.cancelled:
		os.Remove(msg.path)
		m.setStatus("create cancelled", false)
		return nil
	case msg.err != nil && msg.path == "":
		m.setStatus("create: "+msg.err.Error(), true)
		return nil
	case msg.err != nil:
		m.setStatus("manifest: "+msg.err.Error()+" (editing again)", true)
		return m.openEditor(msg.editMsg)
	}
	os.Remove(msg.path)
	if msg.key != m.cur || m.backend == nil {
		m.setStatus("the view changed while editing; create cancelled", true)
		return nil
	}
	m.confirm = &confirm{act: createAction, key: msg.key, targets: []target{{id: msg.id, name: msg.id, obj: msg.obj}}}
	m.mode, m.input = modeConfirm, ""
	return nil
}

// parseManifest reads one protojson-shaped YAML document strictly, so a
// typo is an error rather than a dropped field, and checks what ateapi
// needs before anything is sent.
func parseManifest(k kind, data []byte) (proto.Message, string, error) {
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
