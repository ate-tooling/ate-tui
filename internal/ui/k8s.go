package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The ate CRDs: WorkerPools, SandboxConfigs and CSIDriverConfigs. They are
// read and written with the Kubernetes API and the kubeconfig credentials,
// not through ateapi.

// kubeKind reports whether k lists Kubernetes objects rather than ateapi ones.
func kubeKind(k kind) bool { return k == kWorkerPools || k == kSandboxCfgs || k == kCSIDrivers }

// objID is a row id: namespace/name, or name for a cluster-scoped object.
func objID(o metav1.Object) string {
	if o.GetNamespace() == "" {
		return o.GetName()
	}
	return o.GetNamespace() + "/" + o.GetName()
}

// eventKind is the involvedObject.kind of a CRD's events.
func eventKind(obj any) string {
	switch obj.(type) {
	case *infrav1.WorkerPool:
		return "WorkerPool"
	case *infrav1.SandboxConfig:
		return "SandboxConfig"
	case *infrav1.CSIDriverConfig:
		return "CSIDriverConfig"
	}
	return ""
}

// sandboxConfigUsers are the pools whose status names each SandboxConfig,
// in configRef or pendingConfigRef: the ones that keep it from deleting.
func sandboxConfigUsers(pools []*infrav1.WorkerPool) map[string][]string {
	out := map[string][]string{}
	for _, p := range pools {
		for _, sc := range p.Status.SandboxClasses {
			seen := map[string]bool{}
			for _, ref := range []*infrav1.SandboxConfigReference{sc.ConfigRef, sc.PendingConfigRef} {
				if ref != nil && ref.Name != "" && !seen[ref.Name] {
					seen[ref.Name] = true
					out[ref.Name] = append(out[ref.Name], objID(p))
				}
			}
		}
	}
	return out
}

func fetchSandboxConfigs(ctx context.Context, b Backend, _ string, now time.Time) ([]row, error) {
	var (
		cs      []infrav1.SandboxConfig
		ps      []infrav1.WorkerPool
		err     error
		poolErr error
		wg      sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		cs, err = b.ListSandboxConfigs(ctx)
	}()
	go func() {
		defer wg.Done()
		ps, poolErr = b.ListWorkerPools(ctx)
	}()
	wg.Wait()
	if err != nil {
		return nil, err
	}
	// Without the pools, the POOLS column says it does not know.
	var users map[string][]string
	if poolErr == nil {
		pools := make([]*infrav1.WorkerPool, len(ps))
		for i := range ps {
			pools[i] = &ps[i]
		}
		users = sandboxConfigUsers(pools)
	}
	rows := make([]row, 0, len(cs))
	for i := range cs {
		rows = append(rows, sandboxConfigRow(&cs[i], users, now))
	}
	return rows, nil
}

// sandboxConfigRow renders a SandboxConfig; users nil means the pools are
// not known.
func sandboxConfigRow(c *infrav1.SandboxConfig, users map[string][]string, now time.Time) row {
	enabled := 0
	for _, v := range c.Spec.Versions {
		if v.State == nil || *v.State != infrav1.SandboxVersionStateDisabled {
			enabled++
		}
	}
	classDefault := "-"
	if c.Annotations[infrav1.SandboxConfigClassDefaultAnnotation] == "true" {
		classDefault = "yes"
	}
	pools, n := "?", 0
	if users != nil {
		n = len(users[c.Name])
		pools = fmt.Sprint(n)
	}
	lv, st := lvNormal, "Unused"
	switch {
	case n > 0:
		st = "In use"
	case users == nil:
		st = "-"
	}
	var arches []string
	if def, ok := c.Spec.DefaultVersionConfig(); ok {
		for a := range def.Assets {
			arches = append(arches, a)
		}
		sort.Strings(arches)
	} else {
		lv, st = lvError, "No default"
	}
	if c.DeletionTimestamp != nil {
		lv, st = lvGone, "Terminating"
		if n > 0 {
			lv, st = lvWarn, "Terminating (in use)"
		}
	}
	return newRow(c.Name, c, lv, []string{
		c.Name, dash(string(c.Spec.SandboxClass)), dash(c.Spec.DefaultVersion),
		fmt.Sprintf("%d/%d", enabled, len(c.Spec.Versions)),
		classDefault, pools, st,
		age(c.CreationTimestamp.Time, now),
		dash(strings.Join(arches, ",")), dash(string(c.UID)),
	}, map[int]string{
		5: fmt.Sprintf("%010d", n),
		7: sortTime(c.CreationTimestamp.Time),
	})
}

func csiRow(c *infrav1.CSIDriverConfig, now time.Time) row {
	tls := "off"
	if t := c.Spec.TLS; t != nil && t.Enabled {
		tls = "on " + t.ServerName
	}
	lv, st := lvNormal, "Active"
	if c.DeletionTimestamp != nil {
		lv, st = lvGone, "Terminating"
	}
	sock := c.Spec.NodeSocketOverride
	if sock == "" {
		sock = "unix:///var/lib/kubelet/plugins/" + c.Spec.DriverName + "/csi.sock"
	}
	return newRow(c.Name, c, lv, []string{
		c.Name, dash(c.Spec.DriverName), dash(c.Spec.ControllerEndpoint), tls, st,
		age(c.CreationTimestamp.Time, now),
		sock, dash(string(c.UID)),
	}, map[int]string{5: sortTime(c.CreationTimestamp.Time)})
}

// kubeRow renders a CRD the way its view does. A SandboxConfig gets its
// POOLS from the cached pools, if they are loaded.
func (m *Model) kubeRow(obj any, now time.Time) (row, bool) {
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		return poolRow(o, now), true
	case *infrav1.SandboxConfig:
		var users map[string][]string
		if ps := cachedObjs[*infrav1.WorkerPool](m, viewKey{kWorkerPools, ""}); ps != nil {
			users = sandboxConfigUsers(ps)
		}
		return sandboxConfigRow(o, users, now), true
	case *infrav1.CSIDriverConfig:
		return csiRow(o, now), true
	}
	return row{}, false
}
