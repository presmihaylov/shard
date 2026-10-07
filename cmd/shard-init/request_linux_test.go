package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

// The kernel records the caller at connect, and that record is what the parent check reads.
func TestPeerCredNamesTheCaller(t *testing.T) {
	setRequestAddr(t, fmt.Sprintf("@shard-init-test/peer/%d", os.Getpid()))
	l, err := listenRequests()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closeLater(t, l)
	type accepted struct {
		cred *unix.Ucred
		err  error
	}
	got := make(chan accepted, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			got <- accepted{err: err}

			return
		}
		closeLater(t, conn)
		cred, err := peerCred(conn)
		got <- accepted{cred: cred, err: err}
	}()

	caller, err := net.Dial("unix", requestAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	closeLater(t, caller)
	a := <-got
	if a.err != nil {
		t.Fatalf("read the caller: %v", a.err)
	}
	if int(a.cred.Pid) != os.Getpid() || int(a.cred.Uid) != os.Getuid() {
		t.Fatalf("the caller reads pid %d uid %d, want %d and %d", a.cred.Pid, a.cred.Uid, os.Getpid(), os.Getuid())
	}
}

// A test process has a parent, as every process born in a sandbox does, so root or not it is refused.
func TestAdmitReadsTheParentOffProc(t *testing.T) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", os.Getpid()))
	if err != nil {
		t.Fatalf("read the status: %v", err)
	}

	err = admit(0, int32(os.Getpid()), status)
	if want := fmt.Sprintf("has parent %d", os.Getppid()); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("admit gave %v, want %q", err, want)
	}
}

func TestPeerIsHostRefusesWhatIsNotAUnixSocket(t *testing.T) {
	a, b := net.Pipe()
	closeLater(t, a)
	closeLater(t, b)

	if err := peerIsHost(a); err == nil || !strings.Contains(err.Error(), "is not a unix socket") {
		t.Fatalf("a pipe gave %v, want the refusal", err)
	}
}
