package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

// The name is abstract, so it needs no file and a second PID 1 in the same network namespace cannot take it.
func TestListenRequestsHoldsTheAbstractName(t *testing.T) {
	setRequestAddr(t, fmt.Sprintf("@shard-init-test/listen/%d", os.Getpid()))
	l, err := listenRequests()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closeLater(t, l)

	if again, err := listenRequests(); err == nil {
		closeLater(t, again)
		t.Fatal("a second listener took the name the first one holds")
	}
}

func TestPeerIsRootAdmitsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the caller must be root")
	}
	setRequestAddr(t, fmt.Sprintf("@shard-init-test/peer/%d", os.Getpid()))
	l, err := listenRequests()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closeLater(t, l)
	accepted := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			accepted <- err

			return
		}
		closeLater(t, conn)
		accepted <- peerIsRoot(conn)
	}()

	caller, err := net.Dial("unix", requestAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	closeLater(t, caller)
	if err := <-accepted; err != nil {
		t.Fatalf("root was refused: %v", err)
	}
}

func TestPeerIsRootRefusesWhatIsNotAUnixSocket(t *testing.T) {
	a, b := net.Pipe()
	closeLater(t, a)
	closeLater(t, b)

	if err := peerIsRoot(a); err == nil || !strings.Contains(err.Error(), "is not a unix socket") {
		t.Fatalf("a pipe gave %v, want the refusal", err)
	}
}
