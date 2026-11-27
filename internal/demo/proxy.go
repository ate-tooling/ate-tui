package demo

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ate-tooling/ate-tui/internal/explain"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// PrepareProxy stands in for minting the router client certificate.
func (b *Backend) PrepareProxy(context.Context) error {
	latency()
	return nil
}

// DialActor behaves like a CONNECT through atenet-router: a suspended actor
// is resumed, and one that cannot serve is refused. The far end is a small
// HTTP server that says which actor and port answered.
func (b *Backend) DialActor(_ context.Context, ref *ateapi.ObjectRef, port int) (net.Conn, error) {
	latency()
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.actor(ref)
	if err != nil {
		return nil, fmt.Errorf("router rejected CONNECT to %s:%d with 404 Not Found: %s", ref.GetName(), port, err)
	}
	// A proxied actor keeps still, so the random lifecycle does not
	// suspend it under an open forward.
	b.pinned[a] = true
	switch a.Status.State {
	case ateapi.ActorState_ACTOR_STATE_RUNNING, ateapi.ActorState_ACTOR_STATE_RESUMING:
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		if _, busy := b.pending[a]; !busy {
			b.begin(a, ateapi.ActorState_ACTOR_STATE_RESUMING)
		}
	default:
		return nil, fmt.Errorf("router rejected CONNECT to %s:%d with 503 Service Unavailable: actor is %s",
			ref.GetName(), port, explain.ActorState(a.Status.State))
	}
	client, server := net.Pipe()
	go b.serveHTTP(server, a, port)
	return client, nil
}

// serveHTTP answers requests on c until the client closes it.
func (b *Backend) serveHTTP(c net.Conn, a *ateapi.Actor, port int) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()

		b.mu.Lock()
		id := a.Metadata.Atespace + "/" + a.Metadata.Name
		state := explain.ActorState(a.Status.State)
		if pod := a.Status.GetWorkerAssignment().GetWorkerPod(); pod != "" {
			b.emit(pod, a, "agent", fmt.Sprintf("%s %s from atenet-router", req.Method, req.URL.RequestURI()))
		}
		b.mu.Unlock()

		body := fmt.Sprintf("hello from %s on port %d (%s, demo) at %s\n", id, port, strings.ToLower(state), time.Now().Format(time.RFC3339))
		resp := &http.Response{
			StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1, Request: req,
			Header:        http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
			ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)),
			Close: req.Close,
		}
		if err := resp.Write(c); err != nil || req.Close {
			return
		}
	}
}
