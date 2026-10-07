//go:build linux

package netns

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// listenInNewNamespace makes a network namespace with lo up, listens on its loopback, and answers the namespace's path and the listener.
func listenInNewNamespace(t *testing.T) (string, net.Listener) {
	t.Helper()
	type made struct {
		path string
		l    net.Listener
		err  error
	}
	done := make(chan made, 1)
	go func() {
		// The thread never goes home, so the runtime ends it with this goroutine.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			done <- made{err: fmt.Errorf("unshare: %w", err)}

			return
		}
		fd, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			done <- made{err: fmt.Errorf("open the new namespace: %w", err)}

			return
		}
		t.Cleanup(func() { unix.Close(fd) })
		if err := loopbackUp(); err != nil {
			done <- made{err: err}

			return
		}
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		done <- made{path: fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), fd), l: l, err: err}
	}()
	got := <-done
	if errors.Is(got.err, unix.EPERM) {
		t.Skip("this root may not make a network namespace, as in an unprivileged container")
	}
	if got.err != nil {
		t.Fatalf("make a namespace: %v", got.err)
	}
	t.Cleanup(func() { got.l.Close() })

	return got.path, got.l
}

func loopbackUp() error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open a socket: %w", err)
	}
	defer unix.Close(sock)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("name lo: %w", err)
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("read the flags of lo: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(sock, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bring lo up: %w", err)
	}

	return nil
}

func TestDialInReachesTheLoopbackOfTheNamespaceAndNotTheHosts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("entering a network namespace needs root")
	}
	path, l := listenInNewNamespace(t)
	port := uint16(l.Addr().(*net.TCPAddr).Port) //nolint:gosec // a port fits

	go func() {
		conn, err := l.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)

			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("inside")); err != nil {
			t.Errorf("write: %v", err)
		}
	}()
	conn, err := DialIn(t.Context(), path, port)
	if err != nil {
		t.Fatalf("DialIn: %v", err)
	}
	defer conn.Close()
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "inside" {
		t.Fatalf("read %q, %v; want what the namespace's listener wrote", got, err)
	}

	// The caller's own thread never left home, so the same port on its loopback has nothing behind it.
	if host, err := net.Dial("tcp4", l.Addr().String()); err == nil {
		host.Close()
		t.Fatal("the host's loopback answered on the namespace's port")
	}
}

func TestDialInNamesThePortAndTheNamespaceItCouldNotReach(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("entering a network namespace needs root")
	}

	_, err := DialIn(t.Context(), "/proc/self/ns/missing", 8100)
	if err == nil {
		t.Fatal("DialIn into a missing namespace succeeded")
	}
	for _, want := range []string{"8100", "/proc/self/ns/missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %s", err, want)
		}
	}
}
