package main

import (
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

// echoOnce serves one connection on the loopback the test shares with its supervisor: it reads to EOF, then answers what it read.
func echoOnce(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Errorf("close the listener: %v", err)
		}
	})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)

			return
		}
		defer conn.Close()
		got, err := io.ReadAll(conn)
		if err != nil {
			t.Errorf("read the request: %v", err)
		}
		if _, err := conn.Write(append([]byte("echo:"), got...)); err != nil {
			t.Errorf("answer: %v", err)
		}
	}()

	return uint16(l.Addr().(*net.TCPAddr).Port) //nolint:gosec // a port fits
}

// forwardConn opens a forward port connection once the control port answers, which says every port listens.
func forwardConn(t *testing.T) net.Conn {
	t.Helper()
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	conn, err := dial(ctx, supervisor.ForwardPort)
	if err != nil {
		t.Fatalf("dial the forward port: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn
}

func TestTransportForwardCarriesBytesBothWaysAcrossAHalfClose(t *testing.T) {
	ctx := testContext(t)
	port := echoOnce(t)

	forward, err := supervisor.OpenForward(ctx, forwardConn(t), port)
	if err != nil {
		t.Fatalf("open the forward: %v", err)
	}
	defer forward.Close()
	if _, err := forward.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := forward.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	got, err := io.ReadAll(forward)
	if err != nil || string(got) != "echo:ping" {
		t.Fatalf("read %q, %v; want echo:ping then EOF", got, err)
	}
}

func TestTransportForwardRefusesAPortNothingListensOn(t *testing.T) {
	ctx := testContext(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := uint16(l.Addr().(*net.TCPAddr).Port) //nolint:gosec // a port fits
	if err := l.Close(); err != nil {
		t.Fatalf("close the listener: %v", err)
	}

	if _, err := supervisor.OpenForward(ctx, forwardConn(t), port); !errors.Is(err, syscall.ECONNREFUSED) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("open gave %v, want the guest's connection refused", err)
	}
}

func TestTransportStateSaysTheGuestForwardsPorts(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)

	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	state := awaitKind(t, c, supervisor.KindState)
	if !state.Ports {
		t.Fatalf("state = %+v, want ports", state)
	}
}
