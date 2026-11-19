package demo

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// The ate CRDs behave as the API server and atecontroller would: writes
// check resourceVersion and the schema's rules, a scaled pool rolls one
// replica per poll, and a SandboxConfig a pool uses stays Terminating
// until no pool's status names it. Callers hold b.mu for the helpers.

var gr = func(r string) schema.GroupResource { return infrav1.Resource(r) }

func (b *Backend) seedKube(now time.Time) {
	day := func(d int) metav1.Time { return metav1.NewTime(now.Add(-time.Duration(d) * 24 * time.Hour)) }
	enabled := func(name, arch, asset string) infrav1.SandboxVersionConfig {
		return infrav1.SandboxVersionConfig{Name: name, Assets: map[string]map[string]infrav1.AssetFile{
			arch: {asset: {URL: "gs://ate-sandbox/" + name + "/" + asset + ".tar.zst", SHA256: strings.Repeat(fmt.Sprintf("%x", len(name)%16), 64)}},
		}}
	}
	disabled := infrav1.SandboxVersionStateDisabled
	old := enabled("20260901", "amd64", "gvisor")
	old.State = &disabled
	b.sandboxes = []infrav1.SandboxConfig{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "gvisor-stable", CreationTimestamp: day(40),
				Annotations: map[string]string{infrav1.SandboxConfigClassDefaultAnnotation: "true"},
				Finalizers:  []string{infrav1.SandboxConfigWorkerPoolProtectionFinalizer}},
			Spec: infrav1.SandboxConfigSpec{SandboxClass: infrav1.SandboxClassGvisor, DefaultVersion: "20261015",
				Versions: []infrav1.SandboxVersionConfig{old, enabled("20261015", "amd64", "gvisor")}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "gvisor-canary", CreationTimestamp: day(3)},
			Spec: infrav1.SandboxConfigSpec{SandboxClass: infrav1.SandboxClassGvisor, DefaultVersion: "20261110",
				Versions: []infrav1.SandboxVersionConfig{enabled("20261110", "amd64", "gvisor")}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "microvm-v2", CreationTimestamp: day(20),
				Annotations: map[string]string{infrav1.SandboxConfigClassDefaultAnnotation: "true"},
				Finalizers:  []string{infrav1.SandboxConfigWorkerPoolProtectionFinalizer}},
			Spec: infrav1.SandboxConfigSpec{SandboxClass: infrav1.SandboxClassMicroVM, DefaultVersion: "v2",
				Versions: []infrav1.SandboxVersionConfig{enabled("v2", "amd64", "cloud-hypervisor")}},
		},
	}
	for i := range b.sandboxes {
		b.stamp(&b.sandboxes[i].ObjectMeta)
	}
	b.csiDrivers = []infrav1.CSIDriverConfig{{
		ObjectMeta: metav1.ObjectMeta{Name: "hostpath", CreationTimestamp: day(25)},
		Spec: infrav1.CSIDriverConfigSpec{DriverName: "hostpath.csi.k8s.io", ControllerEndpoint: "dns:///csi-hostpath-controller.ate-system:9000",
			TLS: &infrav1.CSIDriverTLSConfig{Enabled: true, UsePodIdentity: true, ServerName: "csi-hostpath-controller.ate-system.svc"}},
	}}
	b.stamp(&b.csiDrivers[0].ObjectMeta)
	for i := range b.pools {
		p := &b.pools[i]
		b.stamp(&p.ObjectMeta)
		class := infrav1.SandboxClass(strings.TrimPrefix(p.Name, "pool-"))
		p.Spec.SandboxClasses = []infrav1.WorkerPoolSandboxClass{{Name: class}}
		p.Status.Selector = "ate.dev/workerpool=" + p.Name
		cfg := "gvisor-stable"
		if class == infrav1.SandboxClassMicroVM {
			cfg = "microvm-v2"
		}
		p.Status.SandboxClasses[0].ConfigRef = &infrav1.SandboxConfigReference{Name: cfg}
	}
	t := metav1.NewTime(now.Add(-25 * time.Minute))
	b.kubeEvents = []corev1.Event{{
		InvolvedObject: corev1.ObjectReference{Kind: "WorkerPool", Namespace: "ate-workers", Name: "pool-microvm"},
		Type:           corev1.EventTypeWarning, Reason: "FailedCreatePod", Count: 6, FirstTimestamp: t, LastTimestamp: t,
		Message: "0/3 nodes are available: 3 Insufficient devices.kubevirt.io/kvm.",
	}}
}

// stamp gives a new object the fields the API server sets.
func (b *Backend) stamp(md *metav1.ObjectMeta) {
	b.rv++
	md.UID = types.UID(fmt.Sprintf("%08x-%04x-%04x", b.rng.Uint32(), b.rng.Intn(65535), b.rng.Intn(65535)))
	md.ResourceVersion = fmt.Sprint(b.rv)
	md.Generation = 1
	if md.CreationTimestamp.IsZero() {
		md.CreationTimestamp = metav1.Now()
	}
}

// bump records an update to md.
func (b *Backend) bump(md *metav1.ObjectMeta) {
	b.rv++
	md.ResourceVersion = fmt.Sprint(b.rv)
	md.Generation++
}

func (b *Backend) event(kind, namespace, name, reason, msg string) {
	t := metav1.Now()
	b.kubeEvents = append(b.kubeEvents, corev1.Event{
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: namespace, Name: name},
		Type:           corev1.EventTypeNormal, Reason: reason, Message: msg, Count: 1, FirstTimestamp: t, LastTimestamp: t,
	})
}

func (b *Backend) ListSandboxConfigs(context.Context) ([]infrav1.SandboxConfig, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseSandboxes()
	out := make([]infrav1.SandboxConfig, len(b.sandboxes))
	for i := range b.sandboxes {
		out[i] = *b.sandboxes[i].DeepCopy()
	}
	return out, nil
}

func (b *Backend) ListCSIDriverConfigs(context.Context) ([]infrav1.CSIDriverConfig, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]infrav1.CSIDriverConfig, len(b.csiDrivers))
	for i := range b.csiDrivers {
		out[i] = *b.csiDrivers[i].DeepCopy()
	}
	return out, nil
}

// releaseSandboxes is atecontroller's finalizer: a Terminating config goes
// once no pool's status names it.
func (b *Backend) releaseSandboxes() {
	used := map[string]bool{}
	for _, p := range b.pools {
		for _, sc := range p.Status.SandboxClasses {
			if sc.ConfigRef != nil {
				used[sc.ConfigRef.Name] = true
			}
		}
	}
	kept := b.sandboxes[:0]
	for _, c := range b.sandboxes {
		if c.DeletionTimestamp == nil || used[c.Name] {
			kept = append(kept, c)
		}
	}
	b.sandboxes = kept
}

// rollPools moves a written pool one replica toward its spec per poll, and
// resolves its sandbox class as the controller would.
func (b *Backend) rollPools() {
	for i := range b.pools {
		p := &b.pools[i]
		if !b.rolling[p.Name] {
			continue
		}
		step := func(v int32) int32 {
			switch {
			case v < p.Spec.Replicas:
				return v + 1
			case v > p.Spec.Replicas:
				return v - 1
			}
			return v
		}
		p.Status.Replicas = step(p.Status.Replicas)
		p.Status.ReadyReplicas = min(step(p.Status.ReadyReplicas), p.Status.Replicas)
		b.resolve(p)
		if p.Status.Replicas == p.Spec.Replicas && p.Status.ReadyReplicas == p.Spec.Replicas {
			delete(b.rolling, p.Name)
		}
	}
}

// resolve sets the pool's sandbox class status from its spec.
func (b *Backend) resolve(p *infrav1.WorkerPool) {
	if len(p.Spec.SandboxClasses) == 0 {
		return
	}
	sc := p.Spec.SandboxClasses[0]
	st := infrav1.WorkerPoolSandboxClassStatus{Name: sc.Name}
	if len(p.Status.SandboxClasses) > 0 {
		st.ConfigRef = p.Status.SandboxClasses[0].ConfigRef
	}
	cond := metav1.Condition{Type: infrav1.WorkerPoolSandboxClassConditionResolved, Status: metav1.ConditionTrue,
		Reason: infrav1.WorkerPoolReasonResolved, LastTransitionTime: metav1.Now()}
	var cfg *infrav1.SandboxConfig
	if sc.ConfigRef != nil {
		cfg = b.sandbox(sc.ConfigRef.Name)
		switch {
		case cfg == nil:
			cond.Status, cond.Reason = metav1.ConditionFalse, infrav1.WorkerPoolReasonSandboxConfigNotFound
			cond.Message = fmt.Sprintf("SandboxConfig %q not found", sc.ConfigRef.Name)
		case cfg.Spec.SandboxClass != sc.Name:
			cond.Status, cond.Reason = metav1.ConditionFalse, infrav1.WorkerPoolReasonSandboxClassMismatch
			cond.Message = fmt.Sprintf("SandboxConfig %q is of class %s", cfg.Name, cfg.Spec.SandboxClass)
			cfg = nil
		}
	} else {
		for i := range b.sandboxes {
			c := &b.sandboxes[i]
			if c.Spec.SandboxClass == sc.Name && c.Annotations[infrav1.SandboxConfigClassDefaultAnnotation] == "true" && c.DeletionTimestamp == nil &&
				(cfg == nil || c.CreationTimestamp.After(cfg.CreationTimestamp.Time)) {
				cfg = c
			}
		}
		if cfg == nil {
			cond.Status, cond.Reason = metav1.ConditionFalse, infrav1.WorkerPoolReasonDefaultConfigNotFound
			cond.Message = "no SandboxConfig of class " + string(sc.Name) + " is annotated as the class default"
		}
	}
	if cfg != nil {
		if cfg.DeletionTimestamp != nil {
			cond.Status, cond.Reason = metav1.ConditionFalse, infrav1.WorkerPoolReasonSandboxConfigDeleting
			cond.Message = fmt.Sprintf("SandboxConfig %q is being deleted", cfg.Name)
		} else {
			st.ConfigRef = &infrav1.SandboxConfigReference{Name: cfg.Name}
			if !hasString(cfg.Finalizers, infrav1.SandboxConfigWorkerPoolProtectionFinalizer) {
				cfg.Finalizers = append(cfg.Finalizers, infrav1.SandboxConfigWorkerPoolProtectionFinalizer)
			}
		}
	}
	st.Conditions = []metav1.Condition{cond}
	p.Status.SandboxClasses = []infrav1.WorkerPoolSandboxClassStatus{st}
}

func hasString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (b *Backend) sandbox(name string) *infrav1.SandboxConfig {
	for i := range b.sandboxes {
		if b.sandboxes[i].Name == name {
			return &b.sandboxes[i]
		}
	}
	return nil
}

func (b *Backend) pool(namespace, name string) *infrav1.WorkerPool {
	for i := range b.pools {
		if b.pools[i].Namespace == namespace && b.pools[i].Name == name {
			return &b.pools[i]
		}
	}
	return nil
}

func (b *Backend) csi(name string) *infrav1.CSIDriverConfig {
	for i := range b.csiDrivers {
		if b.csiDrivers[i].Name == name {
			return &b.csiDrivers[i]
		}
	}
	return nil
}

var (
	sha256Re   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	endpointRe = regexp.MustCompile(`^(tcp|dns)://.+$`)
)

func invalid(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(infrav1.GroupVersion.WithKind(kind).GroupKind(), name, errs)
}

func validatePool(p *infrav1.WorkerPool) error {
	var errs field.ErrorList
	spec := field.NewPath("spec")
	if p.Spec.Replicas < 0 {
		errs = append(errs, field.Invalid(spec.Child("replicas"), p.Spec.Replicas, "must be greater than or equal to 0"))
	}
	if p.Spec.WorkerImage == "" {
		errs = append(errs, field.Required(spec.Child("workerImage"), ""))
	}
	if len(p.Spec.SandboxClasses) != 1 {
		errs = append(errs, field.Invalid(spec.Child("sandboxClasses"), len(p.Spec.SandboxClasses), "must have exactly 1 item"))
	}
	for i, sc := range p.Spec.SandboxClasses {
		if sc.Name != infrav1.SandboxClassGvisor && sc.Name != infrav1.SandboxClassMicroVM {
			errs = append(errs, field.NotSupported(spec.Child("sandboxClasses").Index(i).Child("name"), sc.Name, []string{"gvisor", "microvm"}))
		}
	}
	return invalid("WorkerPool", p.Name, errs)
}

func validateSandbox(c *infrav1.SandboxConfig) error {
	var errs field.ErrorList
	spec := field.NewPath("spec")
	if c.Spec.SandboxClass == "" {
		c.Spec.SandboxClass = infrav1.SandboxClassGvisor
	}
	if c.Spec.SandboxClass != infrav1.SandboxClassGvisor && c.Spec.SandboxClass != infrav1.SandboxClassMicroVM {
		errs = append(errs, field.NotSupported(spec.Child("sandboxClass"), c.Spec.SandboxClass, []string{"gvisor", "microvm"}))
	}
	if len(c.Spec.Versions) == 0 {
		errs = append(errs, field.Required(spec.Child("versions"), "at least 1 item"))
	}
	if _, ok := c.Spec.DefaultVersionConfig(); !ok && len(c.Spec.Versions) > 0 {
		errs = append(errs, field.Invalid(spec, c.Spec.DefaultVersion, "defaultVersion must name an entry in versions that is not Disabled"))
	}
	for i, v := range c.Spec.Versions {
		for arch, assets := range v.Assets {
			for name, a := range assets {
				p := spec.Child("versions").Index(i).Child("assets").Key(arch).Key(name)
				if a.URL == "" {
					errs = append(errs, field.Required(p.Child("url"), ""))
				}
				if !sha256Re.MatchString(a.SHA256) {
					errs = append(errs, field.Invalid(p.Child("sha256"), a.SHA256, "should match '^[a-f0-9]{64}$'"))
				}
			}
		}
	}
	return invalid("SandboxConfig", c.Name, errs)
}

func validateCSI(c *infrav1.CSIDriverConfig) error {
	var errs field.ErrorList
	spec := field.NewPath("spec")
	if c.Spec.DriverName == "" {
		errs = append(errs, field.Required(spec.Child("driverName"), ""))
	}
	if !endpointRe.MatchString(c.Spec.ControllerEndpoint) {
		errs = append(errs, field.Invalid(spec.Child("controllerEndpoint"), c.Spec.ControllerEndpoint, "should match '^(tcp|dns)://.+$'"))
	}
	if t := c.Spec.TLS; t != nil && t.Enabled && (!t.UsePodIdentity || t.ServerName == "") {
		errs = append(errs, field.Invalid(spec.Child("tls"), "", "tls.usePodIdentity and tls.serverName are required when tls.enabled is true"))
	}
	return invalid("CSIDriverConfig", c.Name, errs)
}

func (b *Backend) CreateObject(_ context.Context, obj metav1.Object) (metav1.Object, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		p := o.DeepCopy()
		if b.pool(p.Namespace, p.Name) != nil {
			return nil, apierrors.NewAlreadyExists(gr("workerpools"), p.Name)
		}
		if err := validatePool(p); err != nil {
			return nil, err
		}
		b.stamp(&p.ObjectMeta)
		p.Status = infrav1.WorkerPoolStatus{Selector: "ate.dev/workerpool=" + p.Name}
		b.resolve(p)
		b.pools = append(b.pools, *p)
		b.rolling[p.Name] = true
		b.event("WorkerPool", p.Namespace, p.Name, "Created", fmt.Sprintf("Created worker Deployment with %d replicas", p.Spec.Replicas))
		return p.DeepCopy(), nil
	case *infrav1.SandboxConfig:
		c := o.DeepCopy()
		if b.sandbox(c.Name) != nil {
			return nil, apierrors.NewAlreadyExists(gr("sandboxconfigs"), c.Name)
		}
		if err := validateSandbox(c); err != nil {
			return nil, err
		}
		b.stamp(&c.ObjectMeta)
		b.sandboxes = append(b.sandboxes, *c)
		return c.DeepCopy(), nil
	case *infrav1.CSIDriverConfig:
		c := o.DeepCopy()
		if b.csi(c.Name) != nil {
			return nil, apierrors.NewAlreadyExists(gr("csidriverconfigs"), c.Name)
		}
		if err := validateCSI(c); err != nil {
			return nil, err
		}
		b.stamp(&c.ObjectMeta)
		b.csiDrivers = append(b.csiDrivers, *c)
		return c.DeepCopy(), nil
	}
	return nil, fmt.Errorf("cannot create %T", obj)
}

// current checks an update's resourceVersion against what is stored.
func current(resource string, stored, in metav1.Object) error {
	if in.GetResourceVersion() != "" && in.GetResourceVersion() != stored.GetResourceVersion() {
		return apierrors.NewConflict(gr(resource), in.GetName(),
			fmt.Errorf("the object has been modified; please apply your changes to the latest version and try again"))
	}
	return nil
}

// keepServerFields carries what an update cannot change.
func keepServerFields(in, stored *metav1.ObjectMeta) {
	in.UID, in.CreationTimestamp, in.DeletionTimestamp = stored.UID, stored.CreationTimestamp, stored.DeletionTimestamp
	in.ResourceVersion, in.Generation = stored.ResourceVersion, stored.Generation
}

func (b *Backend) UpdateObject(_ context.Context, obj metav1.Object) (metav1.Object, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		p := b.pool(o.Namespace, o.Name)
		if p == nil {
			return nil, apierrors.NewNotFound(gr("workerpools"), o.Name)
		}
		if err := current("workerpools", p, o); err != nil {
			return nil, err
		}
		in := o.DeepCopy()
		if err := validatePool(in); err != nil {
			return nil, err
		}
		if len(p.Spec.SandboxClasses) == 1 && len(in.Spec.SandboxClasses) == 1 && p.Spec.SandboxClasses[0].Name != in.Spec.SandboxClasses[0].Name {
			return nil, invalid("WorkerPool", in.Name, field.ErrorList{field.Invalid(field.NewPath("spec", "sandboxClasses"), "", "sandboxClasses names are immutable")})
		}
		keepServerFields(&in.ObjectMeta, &p.ObjectMeta)
		in.Status = p.Status // the status subresource is separate
		b.bump(&in.ObjectMeta)
		*p = *in
		b.resolve(p)
		b.rolling[p.Name] = true
		b.event("WorkerPool", p.Namespace, p.Name, "Updated", "Applied the worker Deployment")
		return p.DeepCopy(), nil
	case *infrav1.SandboxConfig:
		c := b.sandbox(o.Name)
		if c == nil {
			return nil, apierrors.NewNotFound(gr("sandboxconfigs"), o.Name)
		}
		if err := current("sandboxconfigs", c, o); err != nil {
			return nil, err
		}
		in := o.DeepCopy()
		if err := validateSandbox(in); err != nil {
			return nil, err
		}
		keepServerFields(&in.ObjectMeta, &c.ObjectMeta)
		b.bump(&in.ObjectMeta)
		*c = *in
		for i := range b.pools {
			b.resolve(&b.pools[i])
		}
		return c.DeepCopy(), nil
	case *infrav1.CSIDriverConfig:
		c := b.csi(o.Name)
		if c == nil {
			return nil, apierrors.NewNotFound(gr("csidriverconfigs"), o.Name)
		}
		if err := current("csidriverconfigs", c, o); err != nil {
			return nil, err
		}
		in := o.DeepCopy()
		if err := validateCSI(in); err != nil {
			return nil, err
		}
		keepServerFields(&in.ObjectMeta, &c.ObjectMeta)
		b.bump(&in.ObjectMeta)
		*c = *in
		return c.DeepCopy(), nil
	}
	return nil, fmt.Errorf("cannot update %T", obj)
}

func (b *Backend) DeleteObject(_ context.Context, obj metav1.Object) error {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	uidGone := func(resource string, stored metav1.Object) error {
		if stored.GetUID() != obj.GetUID() {
			return apierrors.NewConflict(gr(resource), obj.GetName(), fmt.Errorf("Precondition failed: UID in precondition: %s, UID in object meta: %s", obj.GetUID(), stored.GetUID()))
		}
		return nil
	}
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		p := b.pool(o.Namespace, o.Name)
		if p == nil {
			return apierrors.NewNotFound(gr("workerpools"), o.Name)
		}
		if err := uidGone("workerpools", p); err != nil {
			return err
		}
		for i := range b.pools {
			if &b.pools[i] == p {
				b.pools = append(b.pools[:i], b.pools[i+1:]...)
				break
			}
		}
		return nil
	case *infrav1.SandboxConfig:
		c := b.sandbox(o.Name)
		if c == nil {
			return apierrors.NewNotFound(gr("sandboxconfigs"), o.Name)
		}
		if err := uidGone("sandboxconfigs", c); err != nil {
			return err
		}
		if c.DeletionTimestamp == nil {
			now := metav1.Now()
			c.DeletionTimestamp = &now
			b.bump(&c.ObjectMeta)
		}
		if len(c.Finalizers) == 0 {
			for i := range b.sandboxes {
				if &b.sandboxes[i] == c {
					b.sandboxes = append(b.sandboxes[:i], b.sandboxes[i+1:]...)
					break
				}
			}
			return nil
		}
		for i := range b.pools {
			b.resolve(&b.pools[i])
		}
		b.releaseSandboxes()
		return nil
	case *infrav1.CSIDriverConfig:
		c := b.csi(o.Name)
		if c == nil {
			return apierrors.NewNotFound(gr("csidriverconfigs"), o.Name)
		}
		if err := uidGone("csidriverconfigs", c); err != nil {
			return err
		}
		for i := range b.csiDrivers {
			if &b.csiDrivers[i] == c {
				b.csiDrivers = append(b.csiDrivers[:i], b.csiDrivers[i+1:]...)
				break
			}
		}
		return nil
	}
	return fmt.Errorf("cannot delete %T", obj)
}

func (b *Backend) ScaleWorkerPool(_ context.Context, namespace, name string, replicas int32) (*infrav1.WorkerPool, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pool(namespace, name)
	if p == nil {
		return nil, apierrors.NewNotFound(gr("workerpools"), name)
	}
	if replicas < 0 {
		return nil, invalid("WorkerPool", name, field.ErrorList{field.Invalid(field.NewPath("spec", "replicas"), replicas, "must be greater than or equal to 0")})
	}
	b.event("WorkerPool", namespace, name, "Scaled", fmt.Sprintf("Scaled worker Deployment from %d to %d", p.Spec.Replicas, replicas))
	p.Spec.Replicas = replicas
	b.bump(&p.ObjectMeta)
	b.rolling[p.Name] = true
	return p.DeepCopy(), nil
}

// ObjectEvents serves pod events as PodEvents does, and the events the
// demo recorded for the CRDs.
func (b *Backend) ObjectEvents(ctx context.Context, namespace, kind, name string) ([]corev1.Event, error) {
	if kind == "Pod" {
		return b.PodEvents(ctx, namespace, name)
	}
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []corev1.Event
	for _, e := range b.kubeEvents {
		io := e.InvolvedObject
		if io.Kind == kind && io.Name == name && (namespace == "" || io.Namespace == namespace) {
			out = append(out, e)
		}
	}
	return out, nil
}
