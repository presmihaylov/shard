package vz

import (
	"errors"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// recordConn is a net.Conn that only records a Close, for the connect waiter tests.
type recordConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *recordConn) Close() error {
	c.closed.Store(true)

	return nil
}

// A port nobody answers times out, awaitConnect spawns no goroutine of its own, and an answer that lands after the timeout is closed, not leaked (SHARD-619).
func TestAwaitConnectTimesOutAndClosesALateAnswer(t *testing.T) {
	var late func(net.Conn, error)
	var canceled atomic.Bool
	captured := make(chan struct{})
	start := func(fn func(net.Conn, error)) func() {
		late = fn
		close(captured)

		return func() { canceled.Store(true) }
	}

	before := runtime.NumGoroutine()
	conn, err := awaitConnect(9, 20*time.Millisecond, start)
	if conn != nil || err == nil {
		t.Fatalf("timeout: conn=%v err=%v; want a nil conn and the timeout error", conn, err)
	}
	<-captured
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutine count rose from %d to %d over a timed-out connect", before, after)
	}
	if !canceled.Load() {
		t.Error("the connect handle was not freed after the timeout")
	}

	answer := &recordConn{}
	late(answer, nil)
	if !answer.closed.Load() {
		t.Error("the late answer was not closed")
	}
}

// An answer within the timeout comes back open.
func TestAwaitConnectReturnsAnAnswerWithinTheTimeout(t *testing.T) {
	want := &recordConn{}
	start := func(fn func(net.Conn, error)) func() {
		// The framework calls back after ConnectHandler returns, so fire async to let the waiter park on the receive first.
		go func() {
			time.Sleep(10 * time.Millisecond)
			fn(want, nil)
		}()

		return func() {}
	}

	got, err := awaitConnect(9, time.Second, start)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got != net.Conn(want) {
		t.Errorf("got %v; want the answered connection", got)
	}
	if want.closed.Load() {
		t.Error("the answered connection was closed")
	}
}

// A callback that completes before start returns still reaches the caller, not a false timeout that closes a healthy connection (SHARD-619).
func TestAwaitConnectDeliversAFastAnswer(t *testing.T) {
	want := &recordConn{}
	start := func(fn func(net.Conn, error)) func() {
		fn(want, nil) // the framework can call back before ConnectHandler returns

		return func() {}
	}

	got, err := awaitConnect(9, 20*time.Millisecond, start)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got != net.Conn(want) {
		t.Errorf("got %v; want the answered connection", got)
	}
	if want.closed.Load() {
		t.Error("the answered connection was closed")
	}
}

// Each end of the frames pair holds a burst of full frames nobody reads, where the macOS default refuses the third (SHARD-384).
func TestTheFramesPairHoldsABurstEitherWay(t *testing.T) {
	guest, host, err := frames()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(guest.Close(), host.Close()); err != nil {
			t.Errorf("close the frames pair: %v", err)
		}
	})

	frame := make([]byte, 1514)
	for _, end := range []struct {
		name string
		from *os.File
	}{{"to the guest", host}, {"to the host", guest}} {
		for i := range 1000 {
			if _, err := end.from.Write(frame); err != nil {
				t.Fatalf("frame %d %s: %v", i, end.name, err)
			}
		}
	}
}
