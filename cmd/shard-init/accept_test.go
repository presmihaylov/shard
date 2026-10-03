package main

import (
	"errors"
	"math"
	"net"
	"syscall"
	"testing"
	"time"
)

// failing is a listener whose Accept fails with EMFILE a set number of times, then hands out one end of a pipe.
type failing struct {
	fails, calls int
}

func (l *failing) Accept() (net.Conn, error) {
	l.calls++
	if l.calls <= l.fails {
		return nil, syscall.EMFILE
	}
	conn, _ := net.Pipe()

	return conn, nil
}

func (l *failing) Close() error   { return nil }
func (l *failing) Addr() net.Addr { return &net.UnixAddr{Name: "test", Net: "unix"} }

func TestRetryingAcceptOutlivesAFailedAccept(t *testing.T) {
	inner := &failing{fails: 3}
	conn, err := (&retrying{Listener: inner}).Accept()
	if err != nil {
		t.Fatalf("accept after 3 failures gave %v", err)
	}
	defer conn.Close()
	if inner.calls != 4 {
		t.Fatalf("Accept ran %d times, want 4", inner.calls)
	}
}

func TestRetryingAcceptEndsOnClose(t *testing.T) {
	l := &retrying{Listener: &failing{fails: math.MaxInt}}
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, syscall.EMFILE) {
			t.Fatalf("accept after the close gave %v, want the last failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept still retries 5s after the close")
	}
}
