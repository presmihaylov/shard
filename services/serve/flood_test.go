package serve

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// outOfFiles fails its first Accepts the way a process out of file descriptors does, then hands over to the real listener.
type outOfFiles struct {
	net.Listener
	left atomic.Int32
}

func (l *outOfFiles) Accept() (net.Conn, error) {
	if l.left.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Addr: l.Addr(), Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}

	return l.Listener.Accept()
}

// One EMFILE ended shard serve, so a flood of idle connections took the front down; it now waits the error out (SHARD-372).
func TestTheFrontOutlivesAnAcceptThatRanOutOfFiles(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := frontWith(t, up.root, env.secret, func(_ *Server, listener net.Listener) net.Listener {
		failing := &outOfFiles{Listener: listener}
		failing.left.Store(3)

		return failing
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/v0/sandboxes", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+mint(t, env, "ci"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the front answered nothing after three Accepts ran out of files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the front answered %d, want 200", resp.StatusCode)
	}
}

// A source holds at most its share of the connections that show no token yet, and the next one is closed at once (SHARD-372).
func TestTheFrontClosesAConnectionPastTheCapOfItsSource(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := frontWith(t, up.root, env.secret, func(s *Server, listener net.Listener) net.Listener {
		s.preAuth = newPreAuth(8, 2)

		return listener
	})

	held := []net.Conn{idle(t, address), idle(t, address)}
	if !closedWithin(t, idle(t, address), 2*time.Second) {
		t.Fatal("the front kept a third idle connection from a source whose cap is 2")
	}
	for i, conn := range held {
		if closedWithin(t, conn, 100*time.Millisecond) {
			t.Errorf("the front closed idle connection %d, which is inside the cap", i+1)
		}
	}
}

// A connection leaves the count once its token is accepted, so a client that holds many streams open, as logs -f does, is never refused (SHARD-372).
func TestAnAuthorizedConnectionLeavesTheCap(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := frontWith(t, up.root, env.secret, func(s *Server, listener net.Listener) net.Listener {
		s.preAuth = newPreAuth(8, 2)

		return listener
	})
	header := http.Header{"Authorization": {"Bearer " + mint(t, env, "ci")}}

	var streams []*websocket.Conn
	for i := range 4 {
		conn, _, err := websocket.Dial(t.Context(), "ws://"+address+"/v0/sandboxes/sandbox1/logs?follow=true", &websocket.DialOptions{HTTPHeader: header}) //nolint:bodyclose // a 101 has no body to close
		if err != nil {
			t.Fatalf("authorized stream %d of 4 through a front whose cap is 2: %v", i+1, err)
		}
		defer conn.CloseNow()
		streams = append(streams, conn)
	}

	if closedWithin(t, idle(t, address), 100*time.Millisecond) {
		t.Error("the front closed an idle connection while only authorized streams were open")
	}

	// Each stream ends on the fake daemon's echo, which fails the test if a stream closes first.
	for i, conn := range streams {
		if err := conn.Write(t.Context(), websocket.MessageBinary, []byte("up")); err != nil {
			t.Fatalf("write on stream %d: %v", i+1, err)
		}
		if _, _, err := conn.Read(t.Context()); err != nil {
			t.Fatalf("read the echo on stream %d: %v", i+1, err)
		}
		var closed websocket.CloseError
		if _, _, err := conn.Read(t.Context()); !errors.As(err, &closed) {
			t.Fatalf("stream %d ended with %v, want the fake daemon's close", i+1, err)
		}
	}
}

// A connection that never sends its head is closed once the head timeout ends.
func TestTheFrontClosesAConnectionThatSendsNoHead(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := frontWith(t, up.root, env.secret, func(s *Server, listener net.Listener) net.Listener {
		s.headTimeout = 200 * time.Millisecond

		return listener
	})

	conn := idle(t, address)
	if closedWithin(t, conn, 100*time.Millisecond) {
		t.Fatal("the front closed a silent connection before its head timeout")
	}
	if !closedWithin(t, conn, 2*time.Second) {
		t.Error("the front kept a silent connection past its head timeout")
	}
}

func TestThePreAuthCapCountsInTotalAndPerSource(t *testing.T) {
	gate := newPreAuth(3, 2)
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")

	for i := range 2 {
		if !gate.enter(a) {
			t.Fatalf("connection %d from one source did not fit a per-source cap of 2", i+1)
		}
	}
	if gate.enter(a) {
		t.Fatal("a third connection from one source passed a per-source cap of 2")
	}
	if !gate.enter(b) {
		t.Fatal("a connection from a second source did not fit")
	}
	if gate.enter(b) {
		t.Fatal("a fourth connection passed a total cap of 3")
	}

	gate.leave(a)
	if !gate.enter(b) {
		t.Error("the slot a connection left was not given to the next one")
	}
}

func TestThePreAuthTotalFollowsTheFdLimit(t *testing.T) {
	for _, c := range []struct {
		soft uint64
		want int
	}{
		{soft: 0, want: preAuthPerSource},
		{soft: 127, want: preAuthPerSource},
		{soft: 128, want: 32},
		{soft: 256, want: 96},
		{soft: 4096, want: 2016},
		{soft: 524287, want: 262111},
	} {
		if got := preAuthTotalFor(c.soft); got != c.want {
			t.Errorf("a soft fd limit of %d gave a total cap of %d, want %d", c.soft, got, c.want)
		}
	}

	if got := preAuthTotalFor(math.MaxUint64); got <= 0 {
		t.Errorf("an unlimited soft fd limit gave a total cap of %d", got)
	}
}

// idle opens a TCP connection to the front and sends nothing, as a flood does.
func idle(t *testing.T, address string) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the front: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn
}

// closedWithin reports whether the front closed conn within d.
func closedWithin(t *testing.T, conn net.Conn, d time.Duration) bool {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	_, err := conn.Read(make([]byte, 1))

	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}
