package ui

import (
	"strings"
	"testing"

	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
)

// selectID puts the cursor on the row id of the current view.
func selectID(t *testing.T, m *Model, id string) {
	t.Helper()
	tb := m.table()
	d := m.data[m.cur]
	for i, vi := range tb.visible {
		if d.rows[vi].id == id {
			tb.cursor = i
			return
		}
	}
	t.Fatalf("no row %s in %s", id, m.cur.kind)
}

func TestSandboxConfigsView(t *testing.T) {
	m := demoModel(t, 160, 40)
	press(m, ":", "sandboxconfigs", "enter")
	for id, want := range map[string]string{"gvisor-stable": "1", "gvisor-canary": "0", "microvm-v2": "1"} {
		if got := cell(m, id, 5); got != want {
			t.Errorf("%s POOLS %q, want %q", id, got, want)
		}
	}
	if got := cell(m, "gvisor-stable", 4); got != "yes" {
		t.Errorf("gvisor-stable CLASS DEFAULT %q", got)
	}
	if got := cell(m, "gvisor-stable", 3); got != "1/2" {
		t.Errorf("gvisor-stable VERSIONS %q, want 1/2 (one disabled)", got)
	}
	press(m, ":", "csi", "enter")
	if got := cell(m, "hostpath", 1); got != "hostpath.csi.k8s.io" {
		t.Errorf("csi DRIVER %q", got)
	}
	press(m, ":", "workerpools", "enter")
	if got := cell(m, "ate-workers/pool-gvisor", 6); got != "gvisor-stable" {
		t.Errorf("pool CONFIG %q", got)
	}
}

func TestReadOnlyBlocksKubeWrites(t *testing.T) {
	m := demoModel(t, 120, 40)
	fakeEditor(t, m)
	for _, view := range []string{"workerpools", "sandboxconfigs", "csidriverconfigs"} {
		for _, key := range []string{"ctrl+e", "ctrl+d", "n", "z"} {
			press(m, ":", view, "enter", key)
			if m.mode != modeTable || m.confirm != nil {
				t.Errorf("%s %s: mode %v, want the table", view, key, m.mode)
			}
		}
	}
}

func TestScalePool(t *testing.T) {
	m := writeModel(t)
	press(m, ":", "workerpools", "enter")
	selectID(t, m, "ate-workers/pool-gvisor")
	press(m, "z")
	if m.mode != modeConfirm || m.input != "4" {
		t.Fatalf("mode %v input %q, want the dialog with the current replicas", m.mode, m.input)
	}
	press(m, "ctrl+u", "x", "enter")
	if m.mode != modeConfirm || !m.confirm.refused {
		t.Fatal("a non-number was accepted")
	}
	press(m, "ctrl+u", "6", "enter")
	if m.statusErr || m.status != "scaled ate-workers/pool-gvisor" {
		t.Fatalf("status %q", m.status)
	}
	if got := cell(m, "ate-workers/pool-gvisor", 2); got != "6" {
		t.Errorf("DESIRED %q", got)
	}
	last := m.session[len(m.session)-1]
	if last.Action != "scale" || last.Mode != "replicas=6" || last.Result != "ok" {
		t.Errorf("journal %+v", last)
	}
}

func TestEditPool(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m, func(s string) string {
		return strings.Replace(s, "ateworker:v0.6.0", "ateworker:v0.7.0", 1)
	})
	press(m, ":", "workerpools", "enter")
	selectID(t, m, "ate-workers/pool-gvisor")
	press(m, "ctrl+e")
	got := (*seen)[0]
	if !strings.HasPrefix(got, editHeader) || !strings.Contains(got, "resourceVersion:") ||
		strings.Contains(got, "managedFields") || strings.Contains(got, "readyReplicas") {
		t.Fatalf("edit manifest:\n%s", got)
	}
	if m.mode != modeConfirm || m.confirm.act != editAction {
		t.Fatalf("mode %v", m.mode)
	}
	diff := strings.Join(m.confirm.diff, "\n")
	if !strings.Contains(diff, "- ") || !strings.Contains(diff, "+   workerImage: ghcr.io/agent-substrate/ateworker:v0.7.0") {
		t.Errorf("diff:\n%s", diff)
	}
	if v := m.View(); !strings.Contains(v, "Edit workerpool ate-workers/pool-gvisor?") || !strings.Contains(v, "v0.7.0") {
		t.Errorf("dialog:\n%s", v)
	}
	press(m, "y")
	if m.statusErr || m.status != "edited ate-workers/pool-gvisor" {
		t.Fatalf("status %q", m.status)
	}
	p := m.selected().obj.(*infrav1.WorkerPool)
	if p.Spec.WorkerImage != "ghcr.io/agent-substrate/ateworker:v0.7.0" {
		t.Errorf("image %q", p.Spec.WorkerImage)
	}
}

func TestEditWithoutChangesCancels(t *testing.T) {
	m := writeModel(t)
	fakeEditor(t, m, func(s string) string { return "# a comment\n" + s })
	press(m, ":", "csidriverconfigs", "enter", "ctrl+e")
	if m.mode != modeTable || m.status != "no changes; edit cancelled" {
		t.Errorf("mode %v status %q", m.mode, m.status)
	}
}

func TestEditCannotRename(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m,
		func(s string) string { return strings.Replace(s, "name: hostpath\n", "name: other\n", 1) },
		func(s string) string {
			return strings.Replace(stripErrors(s), "name: other\n", "name: hostpath\n", 1) + "# touched\n"
		},
	)
	press(m, ":", "csidriverconfigs", "enter", "ctrl+e")
	if len(*seen) != 2 || !strings.Contains((*seen)[1], "cannot rename") {
		t.Fatalf("second edit did not show the error:\n%v", *seen)
	}
	if m.mode != modeTable || m.status != "no changes; edit cancelled" {
		t.Errorf("mode %v status %q", m.mode, m.status)
	}
}

func TestCreateSandboxConfigRefusedReopens(t *testing.T) {
	m := writeModel(t)
	sha := strings.Repeat("ab", 32)
	seen := fakeEditor(t, m,
		func(s string) string {
			s = setName("gvisor-next")(s)
			return strings.Replace(s, `url: ""`, "url: gs://gvisor/runsc.tar.zst", 1)
		},
		func(s string) string { return strings.Replace(stripErrors(s), `sha256: ""`, "sha256: "+sha, 1) },
	)
	press(m, ":", "sandboxconfigs", "enter", "n", "y")
	if len(*seen) != 2 || !strings.HasPrefix((*seen)[1], errorPrefix) || !strings.Contains((*seen)[1], "sha256") {
		t.Fatalf("the refused manifest was not edited again:\n%v", *seen)
	}
	if m.mode != modeConfirm {
		t.Fatalf("mode %v", m.mode)
	}
	press(m, "y")
	if m.statusErr || m.status != "created gvisor-next" || m.selectedID() != "gvisor-next" {
		t.Errorf("status %q cursor %q", m.status, m.selectedID())
	}
}

func TestDeleteSandboxConfig(t *testing.T) {
	m := writeModel(t)
	press(m, ":", "sandboxconfigs", "enter")
	// In use: it stays, Terminating, until no pool names it.
	selectID(t, m, "gvisor-stable")
	press(m, "ctrl+d", "gvisor-stable", "enter")
	if m.statusErr || m.status != "deleted gvisor-stable" {
		t.Fatalf("status %q", m.status)
	}
	if got := cell(m, "gvisor-stable", 6); got != "Terminating (in use)" {
		t.Errorf("STATUS %q", got)
	}
	// Unused: gone after the re-poll.
	selectID(t, m, "gvisor-canary")
	press(m, "ctrl+d", "gvisor-canary", "enter")
	if got := cell(m, "gvisor-canary", 0); got != "" {
		t.Errorf("gvisor-canary still listed")
	}
	if last := m.session[len(m.session)-1]; last.Action != "delete" || last.Object != "gvisor-canary" {
		t.Errorf("journal %+v", last)
	}
}

func TestClonePoolAndSandboxConfig(t *testing.T) {
	m := writeModel(t)
	seen := fakeEditor(t, m, func(s string) string { return s }, func(s string) string { return s })
	press(m, ":", "workerpools", "enter")
	selectID(t, m, "ate-workers/pool-gvisor")
	press(m, "ctrl+n")
	got := (*seen)[0]
	for _, bad := range []string{"uid:", "resourceVersion:", "readyReplicas", "creationTimestamp"} {
		if strings.Contains(got, bad) {
			t.Errorf("clone has %s:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "name: pool-gvisor-copy") || !strings.Contains(got, "namespace: ate-workers") {
		t.Errorf("clone manifest:\n%s", got)
	}
	press(m, ":", "sandboxconfigs", "enter")
	selectID(t, m, "gvisor-stable")
	press(m, "ctrl+n")
	got = (*seen)[1]
	if strings.Contains(got, "is-class-default") || strings.Contains(got, "finalizers") {
		t.Errorf("a copy must not be the class default or keep finalizers:\n%s", got)
	}
}

func TestParseKube(t *testing.T) {
	tests := []struct {
		k       kind
		body    string
		wantID  string
		wantErr string
	}{
		{kWorkerPools, "metadata: {namespace: w, name: p}\nspec: {replicas: 1, workerImage: i}", "w/p", ""},
		{kWorkerPools, "metadata: {name: p}", "", "metadata.namespace"},
		{kWorkerPools, "metadata: {namespace: w, name: p}\nspec: {replica: 1}", "", "unknown field"},
		{kWorkerPools, "kind: SandboxConfig\nmetadata: {namespace: w, name: p}", "", "this view makes WorkerPool"},
		{kSandboxCfgs, "apiVersion: ate.dev/v2\nmetadata: {name: s}", "", "apiVersion"},
		{kSandboxCfgs, "metadata: {name: s, namespace: x}", "", "cluster-scoped"},
		{kCSIDrivers, "metadata: {name: c, uid: u, resourceVersion: \"9\"}", "c", ""},
		{kCSIDrivers, "spec: {driverName: d}", "", "metadata.name"},
	}
	for _, tt := range tests {
		obj, id, err := parseKube(tt.k, []byte(tt.body))
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s %q: err %v, want %q", tt.k, tt.body, err, tt.wantErr)
			}
			continue
		}
		if err != nil || id != tt.wantID {
			t.Errorf("%s %q: %v %q %v", tt.k, tt.body, obj, id, err)
		}
		if c, ok := obj.(*infrav1.CSIDriverConfig); ok && (c.UID != "" || c.ResourceVersion != "") {
			t.Error("server fields kept on create")
		}
	}
}

func TestLineDiff(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\n"
	b := "a\nb\nC\nd\ne\nf\ng\nh\n"
	got := strings.Join(lineDiff(a, b), "|")
	want := "  b|- c|+ C|  d|…|  g|+ h"
	if got != want {
		t.Errorf("diff %q, want %q", got, want)
	}
	if d := lineDiff(a, a); d != nil {
		t.Errorf("no change gave %q", d)
	}
}

func TestSandboxConfigRelationsAndEvents(t *testing.T) {
	m := demoModel(t, 160, 40)
	press(m, ":", "workerpools", "enter", ":", "sandboxconfigs", "enter")
	selectID(t, m, "microvm-v2")
	press(m, "u")
	if p := m.pager; p == nil || len(p.items) != 1 || p.items[0].id != "ate-workers/pool-microvm" {
		t.Fatalf("relations %+v", m.pager)
	}
	press(m, "esc", ":", "workerpools", "enter")
	selectID(t, m, "ate-workers/pool-microvm")
	press(m, "E")
	if !strings.Contains(strings.Join(m.pager.lines, "\n"), "FailedCreatePod") {
		t.Errorf("events:\n%s", strings.Join(m.pager.lines, "\n"))
	}
}
