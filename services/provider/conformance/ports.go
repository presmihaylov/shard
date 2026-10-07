package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/portforward"
)

// guestPort is where the port subtests listen, on the sandbox's own loopback, and greeting is what its listener says first.
const (
	guestPort = 8000
	greeting  = "conformance-port"
)

// RunPorts proves a forward to a listener on the sandbox's own loopback; the subject's sandboxes need busybox nc, and the network the substrate forwards into (SHARD-789).
func RunPorts(t *testing.T, s Subject) {
	t.Helper()

	if !s.Provider.Capabilities().Port {
		_, err := s.Provider.DialPort(t.Context(), s.running(t), guestPort)
		s.check(t, models.VerbPort, false, err)
	}

	// A forward reaches a listener bound to the sandbox's own 127.0.0.1, and carries its greeting out and an echo both ways.
	t.Run("DialPortCarriesBytesBothWays", func(t *testing.T) {
		id := s.listening(t)
		conn := greeted(t, func(ctx context.Context) (net.Conn, error) { return s.Provider.DialPort(ctx, id, guestPort) })
		defer conn.Close()
		echoes(t, conn)
	})

	// A stop shuts the host port and a start opens it again onto the new run, as the daemon does with the forward on the record.
	t.Run("AForwardOutlivesAStopAndAStart", func(t *testing.T) {
		id := s.listening(t)
		forward := models.PortForward{HostPort: freePort(t), GuestPort: guestPort}
		ports := s.forwarder(t)
		if err := ports.Open(id, forward); err != nil {
			t.Fatal(err)
		}
		echoesThrough(t, forward.HostPort)

		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := ports.CloseSandbox(id); err != nil {
			t.Fatal(err)
		}
		if conn, err := net.DialTimeout("tcp", hostAddress(forward.HostPort), time.Second); err == nil {
			t.Fatal(errors.Join(fmt.Errorf("host port %d answered while the sandbox was stopped", forward.HostPort), conn.Close()))
		}
		if conn, err := s.Provider.DialPort(t.Context(), id, guestPort); err == nil {
			t.Fatal(errors.Join(errors.New("DialPort reached a stopped sandbox"), conn.Close()))
		}

		if err := s.Provider.Start(t.Context(), id); err != nil {
			t.Fatalf("Start: %v", err)
		}
		s.listen(t, id)
		if err := ports.Open(id, forward); err != nil {
			t.Fatal(err)
		}
		echoesThrough(t, forward.HostPort)
	})
}

// listening starts a sandbox with a listener on guestPort.
func (s Subject) listening(t *testing.T) string {
	t.Helper()

	id := s.running(t)
	s.listen(t, id)

	return id
}

// listen runs a process that greets each connection to guestPort on the sandbox's own loopback, then echoes it.
func (s Subject) listen(t *testing.T, id string) {
	t.Helper()

	s.run(t, id, models.ProcessSpec{Name: "listener", Argv: s.Shell("exec nc -lk -p " + strconv.Itoa(guestPort) + " -s 127.0.0.1 -e /bin/sh -c 'echo " + greeting + "; exec cat'")})
}

// forwarder is the daemon's, over this provider, with no listener left once the test ends.
func (s Subject) forwarder(t *testing.T) *portforward.Forwarder {
	t.Helper()

	ports := portforward.New(s.Provider.DialPort, func(line string) { t.Log(line) }, "")
	t.Cleanup(func() {
		for _, id := range ports.Sandboxes() {
			if err := ports.CloseSandbox(id); err != nil {
				t.Errorf("close the forwards of %s: %v", id, err)
			}
		}
	})

	return ports
}

// echoesThrough waits for the greeting on a forwarded host port, then echoes through it.
func echoesThrough(t *testing.T, hostPort uint16) {
	t.Helper()

	conn := greeted(t, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", hostAddress(hostPort))
	})
	defer conn.Close()
	echoes(t, conn)
}

// echoes sends 256 KiB into the listener and reads the same bytes back, writing and reading at once so neither side's buffer stalls the other.
func echoes(t *testing.T, conn net.Conn) {
	t.Helper()

	sent := make([]byte, 256<<10)
	if _, err := rand.Read(sent); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(waitSlack)); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := conn.Write(sent)
		wrote <- err
	}()
	got := make([]byte, len(sent))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read the echo back: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("write into the sandbox: %v", err)
	}
	if !bytes.Equal(got, sent) {
		t.Fatal("the echo differs from what went in")
	}
}

// greeted dials until the listener greets, since it comes up in its own time and a dial before that is refused.
func greeted(t *testing.T, dial func(context.Context) (net.Conn, error)) net.Conn {
	t.Helper()

	var last error
	for deadline := time.Now().Add(waitSlack); time.Now().Before(deadline); time.Sleep(readyPoll * 10) {
		conn, err := dialGreeting(t.Context(), dial)
		if err == nil {
			return conn
		}
		last = err
	}
	t.Fatalf("the listener on port %d never greeted: %v", guestPort, last)

	return nil
}

func dialGreeting(ctx context.Context, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	line := make([]byte, len(greeting)+1)
	if _, err := io.ReadFull(conn, line); err != nil {
		return nil, errors.Join(fmt.Errorf("read the greeting: %w", err), conn.Close())
	}
	if string(line) != greeting+"\n" {
		return nil, errors.Join(fmt.Errorf("the listener greeted with %q", line), conn.Close())
	}

	return conn, nil
}

// freePort is a host port nothing listens on now; the forwarder binds it a moment later.
func freePort(t *testing.T) uint16 {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, err := netip.ParseAddrPort(l.Addr().String())
	if err != nil {
		t.Fatal(errors.Join(err, l.Close()))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return addr.Port()
}

func hostAddress(port uint16) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
}
