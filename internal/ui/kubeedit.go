package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Editing a CRD opens $EDITOR on the object as kubectl shows it, without
// managedFields and status. The saved manifest replaces the object with an
// Update that carries the resourceVersion it was read at, so a change made
// meanwhile fails with a Conflict instead of being overwritten. The confirm
// dialog shows the diff.

var editAction = &action{name: "edit", past: "edited", pending: "Updating",
	run: func(ctx context.Context, b Backend, obj any, _ actionInput) (any, error) {
		return b.UpdateObject(ctx, obj.(metav1.Object))
	}}

// editHeader starts an edit manifest.
const editHeader = "# Save and quit to apply. Quit without saving, or empty the file, to cancel.\n" +
	"# status and the fields the server sets are ignored. The update fails if the object changed since it was read.\n"

func (m *Model) startEdit() tea.Cmd {
	k := m.cur.kind
	switch {
	case !kubeKind(k):
		m.setStatus("cannot edit "+string(k)+"; editing works on workerpools, sandboxconfigs and csidriverconfigs", true)
		return nil
	case m.backend == nil:
		m.setStatus("not connected", true)
		return nil
	case !m.writable():
		m.setStatus(m.readOnlyHint(), true)
		return nil
	}
	r := m.selected()
	if r == nil {
		return nil
	}
	body, err := kubeYAML(editable(r.obj))
	if err != nil {
		m.setStatus("edit: "+err.Error(), true)
		return nil
	}
	return writeManifest(editMsg{key: m.cur, act: editAction, orig: r.obj}, editHeader+body)
}

// editable is obj with its apiVersion and kind, and without managedFields
// and status, as an edit starts from.
func editable(obj any) metav1.Object {
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		c := o.DeepCopy()
		c.TypeMeta = typeMeta("WorkerPool")
		c.ManagedFields, c.Status = nil, infrav1.WorkerPoolStatus{}
		return c
	case *infrav1.SandboxConfig:
		c := o.DeepCopy()
		c.TypeMeta = typeMeta("SandboxConfig")
		c.ManagedFields = nil
		return c
	case *infrav1.CSIDriverConfig:
		c := o.DeepCopy()
		c.TypeMeta = typeMeta("CSIDriverConfig")
		c.ManagedFields = nil
		return c
	}
	return nil
}

func typeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: infrav1.GroupVersion.String(), Kind: kind}
}

func kubeYAML(obj any) (string, error) {
	if obj == nil {
		return "", fmt.Errorf("not an ate CRD")
	}
	b, err := yaml.Marshal(obj)
	if err != nil {
		return "", err
	}
	// A zero status and creation time say nothing; leave them out.
	s := strings.Replace(string(b), "status:\n  replicas: 0\n", "", 1)
	return strings.Replace(s, "  creationTimestamp: null\n", "", 1), nil
}

// newKube is an empty object of a CRD kind.
func newKube(k kind) (metav1.Object, string) {
	switch k {
	case kWorkerPools:
		return &infrav1.WorkerPool{}, "WorkerPool"
	case kSandboxCfgs:
		return &infrav1.SandboxConfig{}, "SandboxConfig"
	case kCSIDrivers:
		return &infrav1.CSIDriverConfig{}, "CSIDriverConfig"
	}
	return nil, ""
}

// decodeKube reads a manifest strictly, so a typo is an error rather than
// a dropped field, and checks what every object needs. The API server
// validates the spec itself, and a refusal reopens the editor.
func decodeKube(k kind, data []byte) (metav1.Object, error) {
	obj, kindName := newKube(k)
	if obj == nil {
		return nil, fmt.Errorf("cannot create %s", k)
	}
	if err := yaml.UnmarshalStrict(data, obj); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	var t metav1.TypeMeta
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		t = o.TypeMeta
	case *infrav1.SandboxConfig:
		t = o.TypeMeta
	case *infrav1.CSIDriverConfig:
		t = o.TypeMeta
	}
	if t.Kind != "" && t.Kind != kindName {
		return nil, fmt.Errorf("kind is %s; this view makes %s", t.Kind, kindName)
	}
	if t.APIVersion != "" && t.APIVersion != infrav1.GroupVersion.String() {
		return nil, fmt.Errorf("apiVersion is %s; want %s", t.APIVersion, infrav1.GroupVersion)
	}
	if obj.GetName() == "" {
		return nil, fmt.Errorf("metadata.name is required")
	}
	switch {
	case k == kWorkerPools && obj.GetNamespace() == "":
		return nil, fmt.Errorf("metadata.namespace is required")
	case k != kWorkerPools && obj.GetNamespace() != "":
		return nil, fmt.Errorf("a %s is cluster-scoped and has no metadata.namespace", kindName)
	}
	return obj, nil
}

// parseKube reads a create manifest. The fields the server and kubectl
// set are cleared, as for a copy, so the output of kubectl get -o yaml
// works as a start.
func parseKube(k kind, data []byte) (any, string, error) {
	obj, err := decodeKube(k, data)
	if err != nil {
		return nil, "", err
	}
	clearCreateFields(obj)
	return obj, objID(obj), nil
}

// clearCreateFields drops what a new object must not inherit from the one
// its manifest came from: the server-set fields, finalizers (a controller
// adds its own) and kubectl's last-applied-configuration. A copy (ctrl-n)
// also loses the class-default annotation, which cloneKube removes; a
// typed create keeps it, as the starter SandboxConfig offers it.
func clearCreateFields(o metav1.Object) {
	clearServerFields(o)
	o.SetFinalizers(nil)
	ann := o.GetAnnotations()
	delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	if len(ann) == 0 {
		ann = nil
	}
	o.SetAnnotations(ann)
}

func clearServerFields(o metav1.Object) {
	o.SetUID("")
	o.SetResourceVersion("")
	o.SetGeneration(0)
	o.SetCreationTimestamp(metav1.Time{})
	o.SetDeletionTimestamp(nil)
	o.SetDeletionGracePeriodSeconds(nil)
	o.SetManagedFields(nil)
	o.SetOwnerReferences(nil)
	if p, ok := o.(*infrav1.WorkerPool); ok {
		p.Status = infrav1.WorkerPoolStatus{}
	}
}

// parseEdit reads an edited manifest of orig and diffs it against orig. It
// may not rename the object; a removed resourceVersion is put back, so the
// update still fails on a concurrent change.
func parseEdit(orig any, data []byte) (any, string, []string, error) {
	o, ok := orig.(metav1.Object)
	if !ok {
		return nil, "", nil, fmt.Errorf("cannot edit %T", orig)
	}
	k := kindOfObj(orig)
	obj, err := decodeKube(k, data)
	if err != nil {
		return nil, "", nil, err
	}
	if obj.GetName() != o.GetName() || obj.GetNamespace() != o.GetNamespace() {
		return nil, "", nil, fmt.Errorf("an edit cannot rename %s; create a copy with ctrl-n instead", objID(o))
	}
	if obj.GetResourceVersion() == "" {
		obj.SetResourceVersion(o.GetResourceVersion())
	}
	if p, ok := obj.(*infrav1.WorkerPool); ok {
		p.Status = *orig.(*infrav1.WorkerPool).Status.DeepCopy()
	}
	before, err := kubeYAML(editable(orig))
	if err != nil {
		return nil, "", nil, err
	}
	after, err := kubeYAML(editable(obj))
	if err != nil {
		return nil, "", nil, err
	}
	return obj, objID(obj), lineDiff(before, after), nil
}

func kindOfObj(obj any) kind {
	switch obj.(type) {
	case *infrav1.WorkerPool:
		return kWorkerPools
	case *infrav1.SandboxConfig:
		return kSandboxCfgs
	case *infrav1.CSIDriverConfig:
		return kCSIDrivers
	}
	return ""
}

// cloneKube is o as a create manifest: its spec, labels and annotations,
// with "-copy" on its name.
func cloneKube(o metav1.Object) (string, error) {
	c := editable(o)
	if c == nil {
		return "", fmt.Errorf("cannot clone %T", o)
	}
	clearCreateFields(c)
	c.SetName(c.GetName() + "-copy")
	ann := c.GetAnnotations()
	// A copy must not take over as its class's default SandboxConfig.
	delete(ann, infrav1.SandboxConfigClassDefaultAnnotation)
	if len(ann) == 0 {
		ann = nil
	}
	c.SetAnnotations(ann)
	return kubeYAML(c)
}

// starterKube is a commented skeleton of a CRD, filled from the selected
// row where it can be.
func (m *Model) starterKube(k kind) string {
	ns, image, class := m.info.Namespace, "", "gvisor"
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	if r := m.selected(); r != nil {
		switch o := r.obj.(type) {
		case *infrav1.WorkerPool:
			ns, image, class = o.Namespace, o.Spec.WorkerImage, string(o.Spec.DefaultSandboxClass())
		case *infrav1.SandboxConfig:
			class = string(o.Spec.SandboxClass)
		}
	}
	switch k {
	case kWorkerPools:
		return fmt.Sprintf(`# A WorkerPool: a Deployment of worker pods the ate controller manages.
# For every field, press y on a pool, or ctrl-n to start from a copy of one.
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  namespace: %s
  name: ""
spec:
  replicas: 1
  workerImage: %q
  sandboxClasses:
    - name: %s   # gvisor or microvm; it cannot change later
      # configRef:
      #   name: ""   # a SandboxConfig; omitted, the class default is used
`, ns, image, class)
	case kSandboxCfgs:
		return fmt.Sprintf(`# A SandboxConfig: the sandbox binaries a WorkerPool's sandbox class boots.
# It is cluster-scoped. A ValidatingAdmissionPolicy checks the assets per class.
apiVersion: ate.dev/v1
kind: SandboxConfig
metadata:
  name: ""
  # annotations:
  #   sandboxconfig.ate.dev/is-class-default: "true"
spec:
  sandboxClass: %s   # gvisor or microvm
  defaultVersion: v1
  versions:
    - name: v1
      assets:
        amd64:
          gvisor:
            url: ""
            sha256: ""   # 64 lower-case hex characters
`, class)
	case kCSIDrivers:
		return `# A CSIDriverConfig: how ate reaches a CSI driver for actor volumes.
# It is cluster-scoped.
apiVersion: ate.dev/v1
kind: CSIDriverConfig
metadata:
  name: ""
spec:
  driverName: ""            # e.g. hostpath.csi.k8s.io; matches the StorageClass
  controllerEndpoint: ""    # dns:///csi-controller:9000 or tcp://host:port
  # nodeSocketOverride: unix:///var/lib/kubelet/plugins/<driver>/csi.sock
  # tls:
  #   enabled: true
  #   usePodIdentity: true
  #   serverName: ""
`
	}
	return ""
}

// lineDiff lists the changed lines of b against a, "- " and "+ " with a
// line of context around each change, and "…" between distant ones.
func lineDiff(a, b string) []string {
	x := strings.Split(strings.TrimRight(a, "\n"), "\n")
	y := strings.Split(strings.TrimRight(b, "\n"), "\n")
	// lcs[i][j] is the longest common subsequence of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		c    byte // ' ', '-', '+'
		line string
	}
	var ops []op
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			ops = append(ops, op{' ', x[i]})
			i, j = i+1, j+1
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			// A removal goes before the line that replaces it.
			ops = append(ops, op{'-', x[i]})
			i++
		default:
			ops = append(ops, op{'+', y[j]})
			j++
		}
	}
	keep := make([]bool, len(ops))
	changed := false
	for n, o := range ops {
		if o.c != ' ' {
			changed = true
			for d := max(n-1, 0); d <= min(n+1, len(ops)-1); d++ {
				keep[d] = true
			}
		}
	}
	if !changed {
		return nil
	}
	var out []string
	gap := false
	for n, o := range ops {
		if !keep[n] {
			gap = true
			continue
		}
		if gap && len(out) > 0 {
			out = append(out, "…")
		}
		gap = false
		out = append(out, string(o.c)+" "+o.line)
	}
	return out
}
