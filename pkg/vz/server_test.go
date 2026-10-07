package vz

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type failedReplyConn struct{ net.Conn }

func (c failedReplyConn) Write([]byte) (int, error) { return 0, syscall.EPIPE }

func TestFailedConnectReplyClosesTheGuestStream(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() {
		if err := client.Close(); !quiet(err) {
			t.Error(err)
		}
	})
	machine := &fake{state: StateRunning}
	done := make(chan error, 1)
	go func() { done <- serveOne(failedReplyConn{server}, machine, func() {}) }()
	if err := writeFrame(client, request{Verb: "connect", Port: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("the reply failed with %v, want EPIPE", err)
	}
	t.Cleanup(func() {
		if err := machine.guest.Close(); !quiet(err) {
			t.Error(err)
		}
	})
	if err := machine.guest.SetReadDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if _, err := machine.guest.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the failed reply left the guest stream open: %v", err)
	}
}

func TestClientDropClosesASilentGuestStream(t *testing.T) {
	machine := &fake{state: StateRunning}
	client := serve(t, machine)
	conn, err := client.Connect(t.Context(), 5000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); !quiet(err) {
			t.Error(err)
		}
		if err := machine.guest.Close(); !quiet(err) {
			t.Error(err)
		}
	})
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := machine.guest.SetReadDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if _, err := machine.guest.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the client drop left the silent guest stream open: %v", err)
	}
}

type guestEOFConn struct {
	net.Conn
	wrote chan struct{}
}

func (c guestEOFConn) Read([]byte) (int, error) {
	<-c.wrote

	return 0, io.EOF
}

func (c guestEOFConn) Write(p []byte) (int, error) {
	close(c.wrote)

	return c.Conn.Write(p)
}

func TestGuestEOFEndsABlockedWrite(t *testing.T) {
	server, client := net.Pipe()
	host, guest := net.Pipe()
	t.Cleanup(func() {
		for _, conn := range []net.Conn{server, client, host, guest} {
			if err := conn.Close(); !quiet(err) {
				t.Error(err)
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- splice(server, guestEOFConn{Conn: host, wrote: make(chan struct{})}) }()
	if _, err := client.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the guest EOF left a write blocked")
	}
}

// leftGuest sent its answer and closed, so a write to it fails while the answer is still on its way.
type leftGuest struct {
	net.Conn
	failed chan struct{}
	answer io.Reader
}

func (c leftGuest) Read(p []byte) (int, error) {
	<-c.failed
	// A loaded host reads the answer only after the write to the guest has failed.
	time.Sleep(50 * time.Millisecond)

	return c.answer.Read(p)
}

func (c leftGuest) Write([]byte) (int, error) {
	close(c.failed)

	return 0, syscall.EPIPE
}

func TestAGuestThatLeftStillDeliversWhatItSent(t *testing.T) {
	server, client := net.Pipe()
	host, guest := net.Pipe()
	closePeersOnCleanup(t, server, client, host, guest)
	done := make(chan error, 1)
	go func() {
		done <- splice(server, leftGuest{Conn: host, failed: make(chan struct{}), answer: strings.NewReader("exit")})
	}()
	if _, err := client.Write([]byte("stdin close")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "exit" {
		t.Fatalf("the client read %q, want the exit the guest sent before it left", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// silentGuest shut only its read side, so a write to it fails and its open write side sends nothing.
type silentGuest struct{ net.Conn }

func (silentGuest) Write([]byte) (int, error) {
	return 0, syscall.EPIPE
}

func TestAGuestThatShutOnlyItsReadSideEndsTheSpliceWithinTheBound(t *testing.T) {
	server, client := net.Pipe()
	host, guest := net.Pipe()
	closePeersOnCleanup(t, server, client, host, guest)
	done := make(chan error, 1)
	go func() { done <- spliceWithin(server, silentGuest{host}, 50*time.Millisecond) }()
	if _, err := client.Write([]byte("stdin close")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a silent guest that shut only its read side held the splice past its bound")
	}
	for name, peer := range map[string]net.Conn{"shim socket": client, "guest stream": guest} {
		if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("the %s read %v, want the EOF of a socket the splice closed", name, err)
		}
	}
}

func TestPeerDropEndsABlockedWrite(t *testing.T) {
	for _, direction := range []string{"client to guest", "guest to client"} {
		t.Run(direction, func(t *testing.T) {
			server, client := net.Pipe()
			host, guest := net.Pipe()
			closePeersOnCleanup(t, server, client, host, guest)
			done := make(chan error, 1)
			go func() { done <- spliceWithin(server, host, 50*time.Millisecond) }()
			peer := client
			if direction == "guest to client" {
				peer = guest
			}
			if _, err := peer.Write([]byte("the other peer does not read this request")); err != nil {
				t.Fatal(err)
			}
			if err := peer.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("the blocked write ended with %v, want its deadline", err)
				}
			case <-time.After(time.Second):
				t.Fatal("the peer drop left a stream write blocked")
			}
		})
	}
}

func TestStreamWritesResetTheirBoundAfterIdleReads(t *testing.T) {
	server, client := net.Pipe()
	host, guest := net.Pipe()
	closePeersOnCleanup(t, server, client, host, guest)
	bound := 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- spliceWithin(server, host, bound) }()
	for range 3 {
		time.Sleep(2 * bound)
		if _, err := client.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len("data"))
		if _, err := io.ReadFull(guest, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != "data" {
			t.Fatalf("the stream sent %q, want data", got)
		}
	}
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the guest close left the stream open")
	}
}

func closePeersOnCleanup(t *testing.T, peers ...net.Conn) {
	t.Helper()
	t.Cleanup(func() {
		for _, peer := range peers {
			if err := peer.Close(); !quiet(err) {
				t.Error(err)
			}
		}
	})
}
