package ui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// failingCreate fails every CRD create with err.
type failingCreate struct {
	Backend
	err error
}

func (f failingCreate) CreateObject(context.Context, metav1.Object) (metav1.Object, error) {
	return nil, f.err
}

func TestEditConflictReopensOnLatestVersion(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m,
		func(s string) string { return strings.Replace(s, "replicas: 4", "replicas: 5", 1) },
		func(s string) string { return stripErrors(s) + "# touched\n" },
	)
	press(m, ":", "workerpools", "enter")
	selectID(t, m, "ate-workers/pool-gvisor")
	press(m, "ctrl+e")
	if m.mode != modeConfirm {
		t.Fatalf("mode %v", m.mode)
	}
	p := m.confirm.targets[0].obj.(*infrav1.WorkerPool)
	if _, err := m.backend.ScaleWorkerPool(t.Context(), p.Namespace, p.Name, 2); err != nil {
		t.Fatal(err)
	}
	press(m, "y")
	if len(*seen) != 2 {
		t.Fatalf("the editor did not reopen on the conflict: %d runs, status %q", len(*seen), m.status)
	}
	again := (*seen)[1]
	if !strings.HasPrefix(again, errorPrefix) || !strings.Contains(again, "replicas: 5") || !strings.Contains(again, "latest version") {
		t.Fatalf("reopened manifest:\n%s", again)
	}
	if m.mode != modeConfirm {
		t.Fatalf("mode %v status %q", m.mode, m.status)
	}
	// The diff is against the latest version: 2 replicas, not 4.
	diff := strings.Join(m.confirm.diff, "\n")
	if !strings.Contains(diff, "-   replicas: 2") || !strings.Contains(diff, "+   replicas: 5") {
		t.Errorf("diff:\n%s", diff)
	}
	press(m, "y")
	if m.statusErr || m.status != "edited ate-workers/pool-gvisor" {
		t.Fatalf("status %q", m.status)
	}
}

func TestCreateFailureKeepsManifest(t *testing.T) {
	m := writeModel(t)
	m.backend = failingCreate{m.backend, apierrors.NewForbidden(schema.GroupResource{Group: "ate.dev", Resource: "csidriverconfigs"}, "x", nil)}
	fakeEditor(t, m, func(s string) string {
		s = setName("kept-one")(s)
		return strings.Replace(s, `driverName: ""`, "driverName: hostpath.csi.k8s.io", 1)
	})
	press(m, ":", "csidriverconfigs", "enter", "n", "y")
	_, path, ok := strings.Cut(m.status, "your manifest is kept at ")
	if !m.statusErr || !ok {
		t.Fatalf("status %q", m.status)
	}
	defer os.Remove(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), errorPrefix) || !strings.Contains(string(b), "name: kept-one") {
		t.Errorf("kept manifest:\n%s", b)
	}
}

func TestCancelAfterRefusalKeepsManifest(t *testing.T) {
	m := writeModel(t)
	fakeEditor(t, m,
		func(s string) string {
			s = setName("gvisor-next")(s)
			return strings.Replace(s, `url: ""`, "url: gs://gvisor/runsc.tar.zst", 1)
		},
		func(s string) string { return s }, // quit without changes
	)
	press(m, ":", "sandboxconfigs", "enter", "n", "y")
	_, path, ok := strings.Cut(m.status, "your manifest is kept at ")
	if !ok {
		t.Fatalf("status %q", m.status)
	}
	defer os.Remove(path)
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "gs://gvisor/runsc.tar.zst") {
		t.Errorf("kept manifest %q, %v", b, err)
	}
}

func TestGoneMarksDoNotFallBackToCursor(t *testing.T) {
	m := writeModel(t)
	press(m, ":", "workers", "enter", "space")
	marked := m.selectedID()
	d := m.data[m.cur]
	var rows []row
	for _, r := range d.rows {
		if r.id != marked {
			rows = append(rows, r)
		}
	}
	// The rows go away in a refresh, which prunes the marks first.
	m.Update(fetchedMsg{key: m.cur, gen: d.gen, rows: rows, at: time.Now()})
	press(m, "U")
	if m.mode == modeConfirm {
		t.Fatalf("the action fell back to the cursor row %v", m.confirm.targets)
	}
	if !m.statusErr || !strings.Contains(m.status, "no longer exist") {
		t.Errorf("status %q", m.status)
	}
	if len(m.table().marked) != 0 {
		t.Errorf("marks not pruned: %v", m.table().marked)
	}
	// Nothing marked now: the cursor row is the target again.
	press(m, "U")
	if m.mode != modeConfirm || len(m.confirm.targets) != 1 {
		t.Errorf("mode %v", m.mode)
	}
}

func TestMarkedTitleCountsLiveRows(t *testing.T) {
	m := writeModel(t)
	press(m, ":", "workers", "enter", "space", "j", "space")
	if v := m.View(); !strings.Contains(v, "marked 2") {
		t.Fatalf("title:\n%s", v)
	}
	d := m.data[m.cur]
	gone := m.selectedID()
	var rows []row
	for _, r := range d.rows {
		if r.id != gone {
			rows = append(rows, r)
		}
	}
	m.Update(fetchedMsg{key: m.cur, gen: d.gen, rows: rows, at: time.Now()})
	if v := m.View(); !strings.Contains(v, "marked 1") {
		t.Errorf("title:\n%s", v)
	}
	if m.table().marked[gone] {
		t.Error("the gone row's mark would rejoin if it came back")
	}
}

func TestCreateClearsCopyFields(t *testing.T) {
	obj, _, err := parseKube(kCSIDrivers, []byte(`apiVersion: ate.dev/v1
kind: CSIDriverConfig
metadata:
  name: pasted
  uid: abc
  resourceVersion: "7"
  finalizers: [ate.dev/cleanup]
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: "{}"
    team: infra
spec:
  driverName: hostpath.csi.k8s.io
`))
	if err != nil {
		t.Fatal(err)
	}
	o := obj.(metav1.Object)
	if o.GetUID() != "" || o.GetResourceVersion() != "" || len(o.GetFinalizers()) != 0 {
		t.Errorf("server fields kept: %+v", o)
	}
	if ann := o.GetAnnotations(); len(ann) != 1 || ann["team"] != "infra" {
		t.Errorf("annotations %v", ann)
	}
}

func TestJournalSkipsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	if err := appendJournal(path, journalEntry{Action: "suspend", Object: "a"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"action":"` + strings.Repeat("x", 2*maxJournalLine) + "\"}\nnot json\n")
	f.Close()
	if err := appendJournal(path, journalEntry{Action: "resume", Object: "b"}); err != nil {
		t.Fatal(err)
	}
	es, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Object != "a" || es[1].Object != "b" {
		t.Errorf("entries %+v", es)
	}
}

func TestJournalTrimsPastMaxSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	var buf []byte
	pad := strings.Repeat("e", 1024)
	for i := 0; i < maxJournal+500; i++ {
		b, _ := json.Marshal(journalEntry{Time: time.Unix(int64(i), 0), Action: "delete", Object: "old", Error: pad})
		buf = append(append(buf, b...), '\n')
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendJournal(path, journalEntry{Action: "create", Object: "new"}); err != nil {
		t.Fatal(err)
	}
	es, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	// The newest entries that fit in half the size limit are kept, so the
	// next appends do not trim again.
	n := len(es)
	if n < 100 || es[n-1].Object != "new" || !es[n-2].Time.Equal(time.Unix(maxJournal+499, 0)) ||
		!es[0].Time.Equal(time.Unix(int64(maxJournal+500-(n-1)), 0)) {
		t.Errorf("%d entries, first %v last %+v", n, es[0].Time, es[n-1])
	}
	fi, _ := os.Stat(path)
	if fi.Size() > maxJournalBytes/2 {
		t.Errorf("journal not trimmed: %d bytes", fi.Size())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".journal-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}

func TestWriteTempFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(dir, "missing"))
	if p, err := writeTemp("x"); err == nil {
		os.Remove(p)
		t.Fatal("writeTemp succeeded in a missing dir")
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Errorf("left %v", es)
	}
}

func TestSetResourceVersion(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
		ok             bool
	}{
		{"two spaces", "metadata:\n  name: p\n  resourceVersion: \"7\"\nspec: {}\n", "metadata:\n  name: p\n  resourceVersion: \"9\"\nspec: {}\n", true},
		{"four spaces", "metadata:\n    name: p\n    resourceVersion: '7'\n", "metadata:\n    name: p\n    resourceVersion: \"9\"\n", true},
		{"absent", "metadata:\n  name: p\n", "metadata:\n  name: p\n", true},
		{"flow style", "metadata: {name: p, resourceVersion: \"7\"}\n", "metadata: {name: p, resourceVersion: \"7\"}\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := setResourceVersion(tt.in, "9")
			if got != tt.want || ok != tt.ok {
				t.Errorf("got %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// Marking a SandboxConfig as its class's default when another config of the
// class already is warns in the confirm dialog; the default itself does not.
func TestSecondClassDefaultWarns(t *testing.T) {
	makeDefault := func(s string) string {
		return strings.Replace(s, "metadata:\n", "metadata:\n  annotations:\n    sandboxconfig.ate.dev/is-class-default: \"true\"\n", 1)
	}
	m := writeModel(t)
	fakeEditor(t, m, makeDefault)
	press(m, ":", "sandboxconfigs", "enter")
	selectID(t, m, "gvisor-canary")
	press(m, "ctrl+e")
	if m.mode != modeConfirm {
		t.Fatalf("mode %v status %q", m.mode, m.status)
	}
	if v := m.View(); !strings.Contains(v, "gvisor-stable is already the default for class gvisor") {
		t.Errorf("no warning:\n%s", v)
	}

	m = writeModel(t)
	fakeEditor(t, m, func(s string) string {
		return strings.Replace(s, "metadata:\n", "metadata:\n  labels:\n    team: runtime\n", 1)
	})
	press(m, ":", "sandboxconfigs", "enter")
	selectID(t, m, "gvisor-stable")
	press(m, "ctrl+e")
	if m.confirm == nil {
		t.Fatalf("mode %v status %q", m.mode, m.status)
	}
	if len(m.confirm.warnings) != 0 {
		t.Errorf("the default itself warned: %v", m.confirm.warnings)
	}
}
