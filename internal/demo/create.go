package demo

import (
	"context"
	"fmt"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The creates validate what ateapi does and fail with its codes. Callers
// hold b.mu for the helpers below.

func (b *Backend) newMetadata(in *ateapi.ResourceMetadata, scoped bool) (*ateapi.ResourceMetadata, error) {
	if in.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	if scoped && !b.hasAtespace(in.GetAtespace()) {
		return nil, status.Errorf(codes.NotFound, "atespace %q not found", in.GetAtespace())
	}
	now := ts(time.Now())
	return &ateapi.ResourceMetadata{
		Atespace: in.GetAtespace(), Name: in.GetName(),
		Uid:     fmt.Sprintf("%08x-%04x", b.rng.Uint32(), b.rng.Intn(65535)),
		Version: 1, CreateTime: now, UpdateTime: now,
	}, nil
}

func (b *Backend) hasAtespace(name string) bool {
	for _, a := range b.atespaces {
		if a.Metadata.Name == name {
			return true
		}
	}
	return false
}

func exists(kind, atespace, name string) error {
	if atespace != "" {
		name = atespace + "/" + name
	}
	return status.Errorf(codes.AlreadyExists, "%s %s already exists", kind, name)
}

func (b *Backend) CreateAtespace(_ context.Context, a *ateapi.Atespace) (*ateapi.Atespace, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	md, err := b.newMetadata(a.GetMetadata(), false)
	if err != nil {
		return nil, err
	}
	md.Atespace = ""
	if b.hasAtespace(md.Name) {
		return nil, exists("atespace", "", md.Name)
	}
	out := &ateapi.Atespace{Metadata: md}
	b.atespaces = append(b.atespaces, out)
	return proto.Clone(out).(*ateapi.Atespace), nil
}

func (b *Backend) CreateActorTemplate(_ context.Context, t *ateapi.ActorTemplate) (*ateapi.ActorTemplate, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	md, err := b.newMetadata(t.GetMetadata(), true)
	if err != nil {
		return nil, err
	}
	for _, o := range b.templates {
		if o.Metadata.Atespace == md.Atespace && o.Metadata.Name == md.Name {
			return nil, exists("actor template", md.Atespace, md.Name)
		}
	}
	if len(t.GetContainers()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "containers: at least one container is required")
	}
	for i, c := range t.GetContainers() {
		if c.GetName() == "" || c.GetImage() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "containers[%d]: name and image are required", i)
		}
	}
	out := proto.Clone(t).(*ateapi.ActorTemplate)
	out.Metadata = md
	// The golden snapshot is taken in the background; until then the
	// template has no golden tag.
	out.Status = &ateapi.ActorTemplateStatus{}
	b.templates = append(b.templates, out)
	return proto.Clone(out).(*ateapi.ActorTemplate), nil
}

func (b *Backend) CreateActor(_ context.Context, a *ateapi.Actor) (*ateapi.Actor, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	md, err := b.newMetadata(a.GetMetadata(), true)
	if err != nil {
		return nil, err
	}
	if _, err := b.actor(&ateapi.ObjectRef{Atespace: md.Atespace, Name: md.Name}); err == nil {
		return nil, exists("actor", md.Atespace, md.Name)
	}
	ref := a.GetActorTemplate()
	found := false
	for _, t := range b.templates {
		found = found || (t.Metadata.Atespace == ref.GetAtespace() && t.Metadata.Name == ref.GetName())
	}
	if !found {
		return nil, status.Errorf(codes.FailedPrecondition, "actor template %s/%s not found", ref.GetAtespace(), ref.GetName())
	}
	out := proto.Clone(a).(*ateapi.Actor)
	out.Metadata = md
	out.Status = &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}
	b.actors = append(b.actors, out)
	b.pinned[out] = true
	return proto.Clone(out).(*ateapi.Actor), nil
}

func (b *Backend) CreateTag(_ context.Context, t *ateapi.Tag) (*ateapi.Tag, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	md, err := b.newMetadata(t.GetMetadata(), true)
	if err != nil {
		return nil, err
	}
	for _, o := range b.tags {
		if o.Metadata.Atespace == md.Atespace && o.Metadata.Name == md.Name {
			return nil, exists("tag", md.Atespace, md.Name)
		}
	}
	src := t.GetSourceActor()
	if src.GetAtespace() != md.Atespace {
		return nil, status.Errorf(codes.InvalidArgument, "source_actor.atespace %q must match metadata.atespace %q", src.GetAtespace(), md.Atespace)
	}
	a, err := b.actor(src)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "source actor: %v", status.Convert(err).Message())
	}
	out := &ateapi.Tag{Metadata: md, Scope: t.GetScope(), SourceActor: src,
		Status: &ateapi.TagStatus{Snapshot: &ateapi.Snapshot{
			Generation: max(a.Status.GetLastAssignedGeneration(), 1),
			Fidelity:   ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		}}}
	if out.Scope == ateapi.TagScope_TAG_SCOPE_UNSPECIFIED {
		out.Scope = ateapi.TagScope_TAG_SCOPE_ATESPACE
	}
	b.tags = append(b.tags, out)
	return proto.Clone(out).(*ateapi.Tag), nil
}
