package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// fakeEditor answers each editor run with the next edit, given what the
// file held. It records every version it saw.
func fakeEditor(t *testing.T, m *Model, edits ...func(string) string) *[]string {
	t.Helper()
	var seen []string
	m.edit = func(path string, done func(error) tea.Msg) tea.Cmd {
		return func() tea.Msg {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, string(b))
			if len(edits) == 0 {
				t.Fatal("editor opened more often than expected")
			}
			next := edits[0](string(b))
			edits = edits[1:]
			if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
				t.Fatal(err)
			}
			return done(nil)
		}
	}
	return &seen
}

func setName(name string) func(string) string {
	return func(s string) string { return strings.Replace(s, `name: ""`, "name: "+name, 1) }
}

func TestReadOnlyBlocksCreate(t *testing.T) {
	m := demoModel(t, 120, 40)
	fakeEditor(t, m)
	press(m, ":", "atespaces", "enter", "n")
	if !m.statusErr || !strings.Contains(m.status, "read-only") {
		t.Errorf("status %q", m.status)
	}
}

func TestCreateAtespace(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m, setName("billing"))
	press(m, ":", "atespaces", "enter", "n")
	if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], editorHeader) {
		t.Fatalf("editor saw %q", *seen)
	}
	if m.mode != modeConfirm || m.confirm.act != createAction || m.confirm.targets[0].id != "billing" {
		t.Fatalf("mode %v confirm %+v", m.mode, m.confirm)
	}
	if v := m.View(); !strings.Contains(v, "Create atespace billing?") {
		t.Errorf("dialog:\n%s", v)
	}
	press(m, "y")
	if m.statusErr || m.status != "created billing" {
		t.Fatalf("status %q", m.status)
	}
	if m.selectedID() != "billing" {
		t.Errorf("cursor on %q, want the new atespace", m.selectedID())
	}
	last := m.session[len(m.session)-1]
	if last.Action != "create" || last.Object != "billing" || last.Result != "ok" {
		t.Errorf("journal %+v", last)
	}
}

func TestCreateActorFromSelectedTemplate(t *testing.T) {
	m := writeModel(t)
	fakeEditor(t, m, setName("fresh-one"))
	press(m, ":", "actors", "enter")
	sel := m.selected().obj.(*ateapi.Actor)
	press(m, "n", "y")
	id := sel.GetMetadata().GetAtespace() + "/fresh-one"
	if m.statusErr || m.status != "created "+id {
		t.Fatalf("status %q", m.status)
	}
	if cell(m, id, 3) != sel.GetActorTemplate().GetName() || cell(m, id, 2) != "Suspended" {
		t.Errorf("new actor row: template %q state %q", cell(m, id, 3), cell(m, id, 2))
	}
}

func TestBadManifestReopensWithError(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m,
		func(s string) string { return s + "colour: blue\n" },
		func(s string) string {
			s = strings.Replace(s, "colour: blue\n", "", 1)
			return setName("ok-now")(s)
		})
	press(m, ":", "atespaces", "enter", "n")
	if len(*seen) != 2 || !strings.HasPrefix((*seen)[1], errorPrefix) || !strings.Contains((*seen)[1], "colour") {
		t.Fatalf("second edit did not show the error:\n%v", *seen)
	}
	if m.mode != modeConfirm || m.confirm.targets[0].id != "ok-now" {
		t.Errorf("mode %v", m.mode)
	}
}

func TestUnchangedManifestCancels(t *testing.T) {
	m := writeModel(t)
	fakeEditor(t, m, func(s string) string { return s })
	press(m, ":", "tags", "enter", "n")
	if m.mode != modeTable || m.status != "create cancelled" {
		t.Errorf("mode %v status %q", m.mode, m.status)
	}
}

func TestCreateFailureIsReported(t *testing.T) {
	m := writeModel(t)
	fakeEditor(t, m, func(s string) string {
		s = strings.Replace(setName("x")(s), `image: ""`, "image: ghcr.io/example/x:v1", 1)
		return regexp.MustCompile(`atespace: \S+`).ReplaceAllString(s, "atespace: nope")
	})
	press(m, ":", "templates", "enter", "n", "y")
	if !m.statusErr || !strings.Contains(m.status, `atespace "nope" not found`) {
		t.Errorf("status %q", m.status)
	}
	if last := m.session[len(m.session)-1]; last.Action != "create" || last.Result != "error" {
		t.Errorf("journal %+v", last)
	}
}

func TestCloneTemplate(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m, func(s string) string { return s })
	press(m, ":", "templates", "enter")
	id := m.selectedID()
	press(m, "ctrl+n")
	got := (*seen)[0]
	if !strings.Contains(got, "-copy") || strings.Contains(got, "uid:") || strings.Contains(got, "status:") {
		t.Errorf("clone manifest:\n%s", got)
	}
	// Unchanged clone: cancelled, nothing created.
	if m.status != "create cancelled" {
		t.Errorf("status %q", m.status)
	}

	fakeEditor(t, m, func(s string) string { return s + "\n" })
	press(m, "ctrl+n", "y")
	as, name, _ := strings.Cut(id, "/")
	if m.status != "created "+as+"/"+name+"-copy" {
		t.Errorf("status %q", m.status)
	}
}

func TestParseManifest(t *testing.T) {
	tests := []struct {
		k       kind
		body    string
		wantID  string
		wantErr string
	}{
		{kAtespaces, "metadata: {name: a}", "a", ""},
		{kAtespaces, "metadata: {name: a, atespace: b}", "", "has no metadata.atespace"},
		{kActors, "metadata: {atespace: r, name: a}\nactor_template: {name: t}", "r/a", ""},
		{kActors, "metadata: {atespace: r, name: a}", "", "actor_template.name"},
		{kTags, "metadata: {atespace: r, name: g}\nsource_actor: {name: a}\nscope: TAG_SCOPE_PUBLISHED", "r/g", ""},
		{kTags, "metadata: {atespace: r, name: g}\nscope: NOPE\nsource_actor: {name: a}", "", "NOPE"},
		{kTemplates, "metadata: {atespace: r, name: t}\ncontainers: [{name: c}]", "", "image are required"},
		{kTemplates, "metadata: {atespace: r}", "", "metadata.name"},
		{kWorkers, "metadata: {name: w}", "", "cannot create"},
	}
	for _, tt := range tests {
		obj, id, err := parseManifest(tt.k, []byte(tt.body))
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s %q: err %v, want %q", tt.k, tt.body, err, tt.wantErr)
			}
			continue
		}
		if err != nil || id != tt.wantID {
			t.Errorf("%s %q: %v %q %v", tt.k, tt.body, obj, id, err)
		}
		if a, ok := obj.(*ateapi.Actor); ok && a.GetActorTemplate().GetAtespace() != "r" {
			t.Error("actor_template.atespace not defaulted from the actor's")
		}
	}
}

func TestCreateNotOnWorkers(t *testing.T) {
	m := writeModel(t)
	fakeEditor(t, m)
	press(m, ":", "workers", "enter", "n")
	if !m.statusErr || !strings.Contains(m.status, "cannot create workers") {
		t.Errorf("status %q", m.status)
	}
}
