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

// PodLogs streams the logs of a worker pod. A worker pod hosts one actor at a
// time but keeps earlier actors' lines, so callers filter by actor labels.
func (c *Conn) PodLogs(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return c.Kube.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

// PodEvents lists the core/v1 events about a pod, as kubectl describe does.
func (c *Conn) PodEvents(ctx context.Context, namespace, pod string) ([]corev1.Event, error) {
	l, err := c.Kube.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("involvedObject.name", pod),
			fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
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
