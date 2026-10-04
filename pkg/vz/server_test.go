package vz

import (
	"errors"
	"io"
	"net"
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
