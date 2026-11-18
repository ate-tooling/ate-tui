package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseMappings(t *testing.T) {
	tests := []struct {
		in      string
		want    []Mapping
		wantErr string
	}{
		{"", []Mapping{{8000, 80}}, ""},
		{"  ", []Mapping{{8000, 80}}, ""},
		{"8080", []Mapping{{8080, 8080}}, ""},
		{"9000:80 :443", []Mapping{{9000, 80}, {0, 443}}, ""},
		{"9000:80,9001:81", []Mapping{{9000, 80}, {9001, 81}}, ""},
		{"0:80 0:81", []Mapping{{0, 80}, {0, 81}}, ""},
		{"80 80", nil, "duplicate local port 80"},
		{"9000:0", nil, "invalid remote port"},
		{"x:80", nil, "invalid local port"},
		{"70000", nil, "invalid port"},
	}
	for _, tt := range tests {
		got, err := ParseMappings(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseMappings(%q) err = %v, want %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseMappings(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
}

// echoDial dials a TCP echo server, which half-closes like an actor would.
func echoDial(ctx context.Context, port int) (net.Conn, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		defer l.Close()
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", l.Addr().String())
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestForwardCopiesAndCounts(t *testing.T) {
	f, err := Start("a/b", "127.0.0.1", Mapping{0, 80}, echoDial)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Stop()
	c, err := net.Dial("tcp", f.Local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read %q, %v", buf, err)
	}
	waitFor(t, func() bool { s := f.Stats(); return s.Active == 1 && s.In == 5 && s.Out == 5 })
	c.Close()
	waitFor(t, func() bool { return f.Stats().Active == 0 })
	if s := f.Stats(); s.Total != 1 || s.Failed != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestForwardRecordsDialErrors(t *testing.T) {
	f, err := Start("a/b", "127.0.0.1", Mapping{0, 81}, func(context.Context, int) (net.Conn, error) {
		return nil, errors.New("router rejected CONNECT")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Stop()
	c, err := net.Dial("tcp", f.Local)
	if err != nil {
		t.Fatal(err)
	}
	// The forward closes the client when the dial fails.
	_, _ = c.Read(make([]byte, 1))
	c.Close()
	waitFor(t, func() bool { return f.Stats().Failed == 1 })
	if s := f.Stats(); s.LastErr != "router rejected CONNECT" || s.Stopped {
		t.Errorf("stats %+v", s)
	}
}

func TestStopClosesOpenTunnels(t *testing.T) {
	f, err := Start("a/b", "127.0.0.1", Mapping{0, 80}, echoDial)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", f.Local)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, func() bool { return f.Stats().Active == 1 })
	f.Stop()
	if s := f.Stats(); !s.Stopped || s.Active != 0 || s.Err != nil {
		t.Errorf("stats after stop %+v", s)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("client still open after Stop")
	}
	if _, err := net.Dial("tcp", f.Local); err == nil {
		t.Error("port still listening after Stop")
	}
}

func TestStartFailsOnBusyPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := l.Addr().(*net.TCPAddr).Port
	if _, err := Start("a/b", "127.0.0.1", Mapping{p, 80}, echoDial); err == nil {
		t.Error("Start on a busy port succeeded")
	}
}
