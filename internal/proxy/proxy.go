// Package proxy serves local TCP ports that tunnel to an actor port, as
// `kubectl ate proxy actor` does. The dial (through atenet-router, or the
// demo) is passed in, so the forwarding loop does not depend on a cluster.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Defaults match kubectl ate proxy actor.
const (
	DefaultLocalPort  = 8000
	DefaultRemotePort = 80
	DefaultAddress    = "127.0.0.1"
)

// Mapping is one local-port:remote-port pair. Local 0 picks a free port.
type Mapping struct {
	Local, Remote int
}

func (m Mapping) String() string { return fmt.Sprintf("%d:%d", m.Local, m.Remote) }

// ParseMappings reads "[local:]remote" specs separated by spaces or commas,
// with kubectl-ate's rules. No specs means 8000:80.
func ParseMappings(s string) ([]Mapping, error) {
	specs := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
	if len(specs) == 0 {
		return []Mapping{{DefaultLocalPort, DefaultRemotePort}}, nil
	}
	out := make([]Mapping, 0, len(specs))
	seen := map[int]bool{}
	for _, spec := range specs {
		localStr, remoteStr, hasColon := strings.Cut(spec, ":")
		var m Mapping
		var err error
		if !hasColon {
			if m.Remote, err = port(spec, 1); err != nil {
				return nil, fmt.Errorf("invalid port %q: must be 1-65535 or [local:]remote", spec)
			}
			m.Local = m.Remote
		} else {
			if m.Remote, err = port(remoteStr, 1); err != nil {
				return nil, fmt.Errorf("invalid remote port in %q: must be 1-65535", spec)
			}
			if localStr != "" {
				if m.Local, err = port(localStr, 0); err != nil {
					return nil, fmt.Errorf("invalid local port in %q: must be 0-65535", spec)
				}
			}
		}
		if m.Local != 0 {
			if seen[m.Local] {
				return nil, fmt.Errorf("duplicate local port %d", m.Local)
			}
			seen[m.Local] = true
		}
		out = append(out, m)
	}
	return out, nil
}

func port(s string, minPort int) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < minPort || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

// DialFunc opens a connection to port on the forward's actor.
type DialFunc func(ctx context.Context, port int) (net.Conn, error)

// Forward is one listening local port. It runs until Stop or until its
// listener fails.
type Forward struct {
	Actor   string
	Local   string // the bound address, host:port
	Remote  int
	Started time.Time

	lis    net.Listener
	dial   DialFunc
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	done   chan struct{}

	active, total, failed atomic.Int64
	in, out               atomic.Int64

	mu      sync.Mutex
	lastErr string
	errAt   time.Time
	stopErr error
}

// Stats is a snapshot of a forward's counters.
type Stats struct {
	Active, Total, Failed int64
	// In counts bytes from the local client to the actor, Out the reverse.
	In, Out int64
	LastErr string
	ErrAt   time.Time
	// Stopped is set once the listener is closed; Err says why if it
	// was not Stop.
	Stopped bool
	Err     error
}

// Start listens on addr:m.Local and forwards each accepted connection to
// m.Remote with dial.
func Start(actor, addr string, m Mapping, dial DialFunc) (*Forward, error) {
	lis, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(m.Local)))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forward{Actor: actor, Local: lis.Addr().String(), Remote: m.Remote, Started: time.Now(),
		lis: lis, dial: dial, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go f.serve()
	return f, nil
}

func (f *Forward) serve() {
	defer close(f.done)
	var backoff time.Duration
	for {
		c, err := f.lis.Accept()
		if err != nil {
			if f.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				// Out of file descriptors and the like pass: keep the
				// forward and its tunnels, and try again shortly, like
				// net/http does.
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() || isTemporary(err) {
					backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
					f.mu.Lock()
					f.lastErr, f.errAt = "accept: "+err.Error(), time.Now()
					f.mu.Unlock()
					select {
					case <-time.After(backoff):
						continue
					case <-f.ctx.Done():
					}
				} else {
					f.mu.Lock()
					f.stopErr = err
					f.mu.Unlock()
				}
			}
			f.cancel()
			// Free the port, so the forward can be started again.
			_ = f.lis.Close()
			f.wg.Wait()
			return
		}
		backoff = 0
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(c)
		}()
	}
}

func (f *Forward) handle(client net.Conn) {
	defer client.Close()
	f.total.Add(1)
	up, err := f.dial(f.ctx, f.Remote)
	if err != nil {
		if f.ctx.Err() == nil {
			f.failed.Add(1)
			f.mu.Lock()
			f.lastErr, f.errAt = err.Error(), time.Now()
			f.mu.Unlock()
		}
		return
	}
	defer up.Close()
	f.active.Add(1)
	defer f.active.Add(-1)

	stop := context.AfterFunc(f.ctx, func() {
		_ = client.Close()
		_ = up.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	half := func(dst, src net.Conn, n *atomic.Int64) {
		defer wg.Done()
		_, _ = io.Copy(counter{dst, n}, src)
		// Pass the EOF on. A conn that cannot half-close is closed, or
		// the other half would wait on it forever.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go half(up, client, &f.in)
	go half(client, up, &f.out)
	wg.Wait()
}

// counter counts the bytes written through it.
type counter struct {
	io.Writer
	n *atomic.Int64
}

func (c counter) Write(p []byte) (int, error) {
	n, err := c.Writer.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Stop closes the listener and every open tunnel, and waits for them.
func (f *Forward) Stop() {
	f.cancel()
	_ = f.lis.Close()
	<-f.done
}

// Done is closed once the forward has stopped.
func (f *Forward) Done() <-chan struct{} { return f.done }

func (f *Forward) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Stats{Active: f.active.Load(), Total: f.total.Load(), Failed: f.failed.Load(),
		In: f.in.Load(), Out: f.out.Load(), LastErr: f.lastErr, ErrAt: f.errAt, Err: f.stopErr}
	select {
	case <-f.done:
		s.Stopped = true
	default:
	}
	return s
}

// isTemporary reports an Accept error that passes: running out of file
// descriptors or buffers, or a connection reset before it was accepted.
func isTemporary(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ECONNRESET)
}
