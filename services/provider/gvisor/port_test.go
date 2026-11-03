package gvisor_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// streamRunsc hands the port forward one end of a stream pair; the test keeps the other, as the sentry's.
type streamRunsc struct {
	gvisor.RunscStub
	stream net.Conn
}

func (r *streamRunsc) PortForward(context.Context, string, uint16) (net.Conn, error) {
	return r.stream, nil
}

func TestDialPortKeepsTheStreamOpenOnAHalfClose(t *testing.T) {
	ours, sentry := streamPair(t)
	p := newProvider(t)
	p.SetRunsc(&streamRunsc{stream: ours})

	conn, err := p.DialPort(t.Context(), sandboxID, 8080)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("DialPort answered a %T, which takes no half close", conn)
	}
	if err := half.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	if err := sentry.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := sentry.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the sentry's end read %v, want no EOF, which would end the connection before its answer", err)
	}
	if _, err := sentry.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("answer"))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "answer" {
		t.Errorf("read %q, %v after the half close, want the answer", got, err)
	}
}

// streamPair is a loopback TCP pair, since a unix socket path under the test's temp dir is too long on a Mac; both take a half close.
func streamPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dialed, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dialed.Close() })
	accepted, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accepted.Close() })

	return dialed, accepted
}
