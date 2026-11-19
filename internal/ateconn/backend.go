package ateconn

import (
	"context"
	"fmt"
	"io"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	infrav1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

const pageSize = 500

// listAll drains a paged List call.
func listAll[T any](fetch func(token string) ([]T, string, error)) ([]T, error) {
	var out []T
	token := ""
	for {
		items, next, err := fetch(token)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

func opts(token string) *ateapi.ListOptions {
	return &ateapi.ListOptions{PageSize: pageSize, PageToken: token}
}

func (c *Conn) ListAtespaces(ctx context.Context) ([]*ateapi.Atespace, error) {
	return listAll(func(t string) ([]*ateapi.Atespace, string, error) {
		r, err := c.Control.ListAtespaces(ctx, &ateapi.ListAtespacesRequest{Options: opts(t)})
		return r.GetAtespaces(), r.GetNextPageToken(), err
	})
}

// ListActors lists actors in atespace, or in every atespace when it is empty.
func (c *Conn) ListActors(ctx context.Context, atespace string) ([]*ateapi.Actor, error) {
	return listAll(func(t string) ([]*ateapi.Actor, string, error) {
		r, err := c.Control.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: atespace, Options: opts(t)})
		return r.GetActors(), r.GetNextPageToken(), err
	})
}

// ListActorTemplates lists templates in atespace. ListActorTemplates requires
// an atespace, so an empty one fans out over every atespace.
func (c *Conn) ListActorTemplates(ctx context.Context, atespace string) ([]*ateapi.ActorTemplate, error) {
	spaces := []string{atespace}
	if atespace == "" {
		all, err := c.ListAtespaces(ctx)
		if err != nil {
			return nil, err
		}
		spaces = spaces[:0]
		for _, a := range all {
			spaces = append(spaces, a.GetMetadata().GetName())
		}
	}
	var out []*ateapi.ActorTemplate
	for _, s := range spaces {
		ts, err := listAll(func(t string) ([]*ateapi.ActorTemplate, string, error) {
			r, err := c.Control.ListActorTemplates(ctx, &ateapi.ListActorTemplatesRequest{Atespace: s, Options: opts(t)})
			return r.GetActorTemplates(), r.GetNextPageToken(), err
		})
		if err != nil {
			return nil, fmt.Errorf("atespace %s: %w", s, err)
		}
		out = append(out, ts...)
	}
	return out, nil
}

func (c *Conn) ListTags(ctx context.Context, atespace string) ([]*ateapi.Tag, error) {
	return listAll(func(t string) ([]*ateapi.Tag, string, error) {
		r, err := c.Control.ListTags(ctx, &ateapi.ListTagsRequest{Atespace: atespace, Options: opts(t)})
		return r.GetTags(), r.GetNextPageToken(), err
	})
}

func (c *Conn) ListWorkers(ctx context.Context) ([]*ateapi.Worker, error) {
	return listAll(func(t string) ([]*ateapi.Worker, string, error) {
		r, err := c.Control.ListWorkers(ctx, &ateapi.ListWorkersRequest{Options: opts(t)})
		return r.GetWorkers(), r.GetNextPageToken(), err
	})
}

func (c *Conn) ListWorkerAssignments(ctx context.Context, worker string) ([]*ateapi.ActorAssignment, error) {
	return listAll(func(t string) ([]*ateapi.ActorAssignment, string, error) {
		r, err := c.Control.ListWorkerActorAssignments(ctx, &ateapi.ListWorkerActorAssignmentsRequest{
			Worker: &ateapi.ObjectRef{Name: worker}, Options: opts(t),
		})
		return r.GetActorAssignments(), r.GetNextPageToken(), err
	})
}

func (c *Conn) GetActorTemplate(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.ActorTemplate, error) {
	return c.Control.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: ref})
}

// ListWorkerPools lists WorkerPools in every namespace.
func (c *Conn) ListWorkerPools(ctx context.Context) ([]infrav1.WorkerPool, error) {
	l, err := c.Ate.AteV1().WorkerPools(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// ListSandboxConfigs lists the cluster-scoped SandboxConfigs.
func (c *Conn) ListSandboxConfigs(ctx context.Context) ([]infrav1.SandboxConfig, error) {
	l, err := c.Ate.AteV1().SandboxConfigs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// ListCSIDriverConfigs lists the cluster-scoped CSIDriverConfigs.
func (c *Conn) ListCSIDriverConfigs(ctx context.Context) ([]infrav1.CSIDriverConfig, error) {
	l, err := c.Ate.AteV1().CSIDriverConfigs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// PodLogs streams the logs of a worker pod. A worker pod hosts one actor at a
// time but keeps earlier actors' lines, so callers filter by actor labels.
func (c *Conn) PodLogs(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return c.Kube.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

// PodEvents lists the core/v1 events about a pod, as kubectl describe does.
func (c *Conn) PodEvents(ctx context.Context, namespace, pod string) ([]corev1.Event, error) {
	return c.ObjectEvents(ctx, namespace, "Pod", pod)
}

// ObjectEvents lists the core/v1 events about an object. A cluster-scoped
// object has an empty namespace, and its events are looked up in every
// namespace, since recorders put them in "default".
func (c *Conn) ObjectEvents(ctx context.Context, namespace, kind, name string) ([]corev1.Event, error) {
	l, err := c.Kube.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("involvedObject.name", name),
			fields.OneTermEqualSelector("involvedObject.kind", kind),
		).String(),
	})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (c *Conn) GetActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	return c.Control.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
}

// singleton is the only name ateapi accepts for an EgressPolicy or an
// AccessPolicy.
const singleton = "default"

func (c *Conn) GetActorEgressPolicy(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.EgressPolicy, error) {
	return c.Control.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: ref, Name: singleton})
}

func (c *Conn) GetGlobalAccessPolicy(ctx context.Context) (*ateapi.AccessPolicy, error) {
	return c.Control.GetGlobalAccessPolicy(ctx, &ateapi.GetGlobalAccessPolicyRequest{Name: singleton})
}

func (c *Conn) GetAtespaceAccessPolicy(ctx context.Context, atespace string) (*ateapi.AccessPolicy, error) {
	return c.Control.GetAtespaceAccessPolicy(ctx, &ateapi.GetAtespaceAccessPolicyRequest{
		Atespace: &ateapi.ObjectRef{Name: atespace}, Name: singleton,
	})
}

// Writes. Each is one RPC; the UI gates them behind --write and a confirm.

func (c *Conn) SuspendActor(ctx context.Context, ref *ateapi.ObjectRef, mode ateapi.DurabilityMode) (*ateapi.Actor, error) {
	r, err := c.Control.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref, Options: &ateapi.SuspendOptions{Mode: mode}})
	return r.GetActor(), err
}

func (c *Conn) ResumeActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	r, err := c.Control.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
	return r.GetActor(), err
}

func (c *Conn) RevertActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	r, err := c.Control.RevertActor(ctx, &ateapi.RevertActorRequest{Actor: ref})
	return r.GetActor(), err
}

func (c *Conn) DeleteActor(ctx context.Context, ref *ateapi.ObjectRef) (*ateapi.Actor, error) {
	return c.Control.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref})
}

func (c *Conn) DrainWorker(ctx context.Context, name string) (*ateapi.Worker, error) {
	r, err := c.Control.DrainWorker(ctx, &ateapi.DrainWorkerRequest{Worker: &ateapi.ObjectRef{Name: name}})
	return r.GetWorker(), err
}

func (c *Conn) MarkWorkerAvailable(ctx context.Context, name string) (*ateapi.Worker, error) {
	r, err := c.Control.MarkWorkerAvailable(ctx, &ateapi.MarkWorkerAvailableRequest{Worker: &ateapi.ObjectRef{Name: name}})
	return r.GetWorker(), err
}

func (c *Conn) MarkWorkerUnavailable(ctx context.Context, name string) (*ateapi.Worker, error) {
	r, err := c.Control.MarkWorkerUnavailable(ctx, &ateapi.MarkWorkerUnavailableRequest{Worker: &ateapi.ObjectRef{Name: name}})
	return r.GetWorker(), err
}

func (c *Conn) CreateAtespace(ctx context.Context, a *ateapi.Atespace) (*ateapi.Atespace, error) {
	return c.Control.CreateAtespace(ctx, &ateapi.CreateAtespaceRequest{Atespace: a})
}

func (c *Conn) CreateActorTemplate(ctx context.Context, t *ateapi.ActorTemplate) (*ateapi.ActorTemplate, error) {
	return c.Control.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: t})
}

func (c *Conn) CreateActor(ctx context.Context, a *ateapi.Actor) (*ateapi.Actor, error) {
	return c.Control.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: a})
}

func (c *Conn) CreateTag(ctx context.Context, t *ateapi.Tag) (*ateapi.Tag, error) {
	return c.Control.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: t})
}

// Kubernetes writes. obj is a *WorkerPool, *SandboxConfig or
// *CSIDriverConfig; the calls use the kubeconfig credentials, not ateapi's.

func (c *Conn) CreateObject(ctx context.Context, obj metav1.Object) (metav1.Object, error) {
	v1 := c.Ate.AteV1()
	co := metav1.CreateOptions{FieldManager: fieldManager}
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		return asObject(v1.WorkerPools(o.Namespace).Create(ctx, o, co))
	case *infrav1.SandboxConfig:
		return asObject(v1.SandboxConfigs().Create(ctx, o, co))
	case *infrav1.CSIDriverConfig:
		return asObject(v1.CSIDriverConfigs().Create(ctx, o, co))
	}
	return nil, fmt.Errorf("cannot create %T", obj)
}

// UpdateObject replaces obj. Its resourceVersion makes the API server
// refuse the update with a Conflict when the object changed since it was read.
func (c *Conn) UpdateObject(ctx context.Context, obj metav1.Object) (metav1.Object, error) {
	v1 := c.Ate.AteV1()
	uo := metav1.UpdateOptions{FieldManager: fieldManager}
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		return asObject(v1.WorkerPools(o.Namespace).Update(ctx, o, uo))
	case *infrav1.SandboxConfig:
		return asObject(v1.SandboxConfigs().Update(ctx, o, uo))
	case *infrav1.CSIDriverConfig:
		return asObject(v1.CSIDriverConfigs().Update(ctx, o, uo))
	}
	return nil, fmt.Errorf("cannot update %T", obj)
}

// DeleteObject deletes obj, and only the object that was read: the UID
// precondition fails if it was deleted and created again meanwhile.
func (c *Conn) DeleteObject(ctx context.Context, obj metav1.Object) error {
	v1 := c.Ate.AteV1()
	uid := obj.GetUID()
	do := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	switch o := obj.(type) {
	case *infrav1.WorkerPool:
		return v1.WorkerPools(o.Namespace).Delete(ctx, o.Name, do)
	case *infrav1.SandboxConfig:
		return v1.SandboxConfigs().Delete(ctx, o.Name, do)
	case *infrav1.CSIDriverConfig:
		return v1.CSIDriverConfigs().Delete(ctx, o.Name, do)
	}
	return fmt.Errorf("cannot delete %T", obj)
}

// ScaleWorkerPool sets spec.replicas with a merge patch, as kubectl scale
// does through the scale subresource.
func (c *Conn) ScaleWorkerPool(ctx context.Context, namespace, name string, replicas int32) (*infrav1.WorkerPool, error) {
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	return c.Ate.AteV1().WorkerPools(namespace).Patch(ctx, name, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager})
}

const fieldManager = "ate-tui"

// asObject keeps a typed nil from becoming a non-nil interface.
func asObject[T any, P interface {
	*T
	metav1.Object
}](o P, err error) (metav1.Object, error) {
	if o == nil {
		return nil, err
	}
	return o, err
}
