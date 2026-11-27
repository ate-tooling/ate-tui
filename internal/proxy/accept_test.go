package proxy

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// flakyListener fails its first Accept with err, then accepts from the real
// listener.
type flakyListener struct {
	net.Listener
	err error
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if err := l.err; err != nil {
		l.err = nil
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", err)}
	}
	return l.Listener.Accept()
}

func startWith(t *testing.T, lis net.Listener, dial DialFunc) *Forward {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forward{Local: lis.Addr().String(), lis: lis, dial: dial, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go f.serve()
	t.Cleanup(f.Stop)
	return f
}

// Running out of file descriptors passes: the forward keeps serving.
func TestAcceptRetriesTemporaryErrors(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dialed := make(chan struct{}, 1)
	f := startWith(t, &flakyListener{Listener: real, err: syscall.EMFILE}, func(context.Context, int) (net.Conn, error) {
		dialed <- struct{}{}
		return nil, errors.New("no actor")
	})
	c, err := net.Dial("tcp", f.Local)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward stopped accepting after EMFILE")
	}
	if s := f.Stats(); s.Stopped {
		t.Fatalf("stopped: %v", s.Err)
	}
}

// A fatal Accept error stops the forward and frees its port.
func TestAcceptFatalErrorFreesPort(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := startWith(t, &flakyListener{Listener: real, err: syscall.EINVAL}, nil)
	select {
	case <-f.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not stop")
	}
	if s := f.Stats(); !s.Stopped || s.Err == nil {
		t.Fatalf("stats = %+v, want stopped with an error", s)
	}
	l, err := net.Listen("tcp", f.Local)
	if err != nil {
		t.Fatalf("port still bound: %v", err)
	}
	l.Close()
}
