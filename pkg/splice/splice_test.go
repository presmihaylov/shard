package splice_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/splice"
)

// pair is one TCP connection over loopback, both ends, so half closes are real.
func pair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
		}
		accepted <- conn
	}()
	dialed, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { dialed.Close() })
	served := <-accepted
	t.Cleanup(func() { served.Close() })

	return dialed, served
}

// spliced runs Conns between the served ends of two pairs and answers the two outer ends and the result.
func spliced(t *testing.T) (net.Conn, net.Conn, <-chan error) {
	t.Helper()
	client, near := pair(t)
	far, server := pair(t)
	done := make(chan error, 1)
	go func() { done <- splice.Conns(near, far) }()

	return client, server, done
}

func TestConnsCarriesAHalfCloseAndTheAnswerAfterIt(t *testing.T) {
	client, server, done := spliced(t)

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	got, err := io.ReadAll(server)
	if err != nil || string(got) != "ping" {
		t.Fatalf("server read %q, %v; want ping then EOF", got, err)
	}
	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatalf("answer after the half close: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close the server: %v", err)
	}
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "pong" {
		t.Fatalf("client read %q, %v; want pong then EOF", got, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("splice: %v", err)
	}
}

func TestConnsMovesMoreThanOneBufferEachWay(t *testing.T) {
	client, server, done := spliced(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10)

	go func() {
		if _, err := client.Write(payload); err != nil {
			t.Errorf("client write: %v", err)
		}
		if err := client.(*net.TCPConn).CloseWrite(); err != nil {
			t.Errorf("client close write: %v", err)
		}
	}()
	got, err := io.ReadAll(server)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("server read %d bytes, %v; want %d", len(got), err, len(payload))
	}
	if _, err := server.Write(payload); err != nil {
		t.Fatalf("server write: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close the server: %v", err)
	}
	got, err = io.ReadAll(client)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("client read %d bytes, %v; want %d", len(got), err, len(payload))
	}
	if err := <-done; err != nil {
		t.Fatalf("splice: %v", err)
	}
}

// A far side with no half close, a pipe here, ends the whole splice on the near side's EOF rather than waiting on it.
func TestConnsClosesBothWhenTheFarSideTakesNoHalfClose(t *testing.T) {
	client, near := pair(t)
	far, remote := net.Pipe()
	defer remote.Close()
	done := make(chan error, 1)
	go func() { done <- splice.Conns(near, far) }()

	if err := client.Close(); err != nil {
		t.Fatalf("close the client: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("splice: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the splice still waits on a far side that cannot hear the EOF")
	}
	if _, err := remote.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the far side read %v, want EOF from the close", err)
	}
}
