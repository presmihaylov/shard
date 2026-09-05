package api

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shortRoot skips t.TempDir, whose path carries the test name past the 104 bytes a macOS socket path allows.
func shortRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "shard") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})

	return root
}

func socketMode(t *testing.T, path string) fs.FileMode {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if info.Mode().Type() != fs.ModeSocket {
		t.Fatalf("%s is a %s, want a socket", path, info.Mode().Type())
	}

	return info.Mode().Perm()
}

func TestListenIsRootOnlyWithoutTheGroup(t *testing.T) {
	root := shortRoot(t)

	listener, mode, group, err := listen(root, "no-such-group-on-any-host")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	if mode != 0o600 || group != "" {
		t.Errorf("listen reported mode %04o and group %q, want 0600 and no group", mode, group)
	}
	if got := socketMode(t, filepath.Join(root, SocketFile)); got != 0o600 {
		t.Errorf("the socket sits at %04o, want 0600", got)
	}
}

func TestListenGivesTheSocketToTheGroupAt0660(t *testing.T) {
	root := shortRoot(t)

	// The caller's own group stands in for shard: chown to it needs no root on any host.
	own, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skipf("no name for the gid %d: %v", os.Getgid(), err)
	}

	listener, mode, group, err := listen(root, own.Name)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	if mode != 0o660 || group != own.Name {
		t.Errorf("listen reported mode %04o and group %q, want 0660 and %s", mode, group, own.Name)
	}

	path := filepath.Join(root, SocketFile)
	if got := socketMode(t, path); got != 0o660 {
		t.Errorf("the socket sits at %04o, want 0660", got)
	}
}

func TestListenRemovesAStaleSocket(t *testing.T) {
	root := shortRoot(t)
	path := filepath.Join(root, SocketFile)

	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("plant the stale socket: %v", err)
	}
	// A daemon that was killed leaves the file behind, which Close would otherwise unlink.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatalf("close the stale listener: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the stale socket is not on disk: %v", err)
	}

	listener, _, _, err := listen(root, "no-such-group-on-any-host")
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	defer listener.Close()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the new socket does not answer: %v", err)
	}
	conn.Close()
}

func TestListenRefusesToRemoveWhatIsNotASocket(t *testing.T) {
	root := shortRoot(t)
	path := filepath.Join(root, SocketFile)

	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("plant the file: %v", err)
	}

	_, _, _, err := listen(root, "no-such-group-on-any-host")
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("listen over a regular file got %v, want a refusal", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file is gone: %v", err)
	}
}

func TestServeEndsWithTheContextAndTakesTheSocketWithIt(t *testing.T) {
	root := shortRoot(t)
	path := filepath.Join(root, SocketFile)

	listener, _, _, err := listen(root, "no-such-group-on-any-host")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	}()

	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get("http://shard/anything")
	if err != nil {
		t.Fatalf("GET over the socket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("the handler answered %d, want 204", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve ended with %v, want nil once the context ended", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context ended")
	}

	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket file is still there after the daemon ended: %v", err)
	}
}

func TestServeReturnsAnErrorWhenTheListenerDies(t *testing.T) {
	root := shortRoot(t)

	listener, _, _, err := listen(root, "no-such-group-on-any-host")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- Serve(t.Context(), listener, http.NotFoundHandler()) }()

	if err := listener.Close(); err != nil {
		t.Fatalf("close the listener: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve returned nil after the listener died, want an error so the daemon restarts it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the listener died")
	}
}
