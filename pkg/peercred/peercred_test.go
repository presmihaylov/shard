package peercred_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/pkg/peercred"
)

// A listener that never accepts still owns the connection the kernel queued for it, so a stopped peer is named all the same.
func TestPIDNamesAListenerThatNeverAccepts(t *testing.T) {
	root, err := os.MkdirTemp("", "peer") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	ln, err := net.Listen("unix", filepath.Join(root, "peer.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	conn, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pid, err := peercred.PID(conn)
	if err != nil {
		t.Fatalf("PID = %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("PID = %d, want %d", pid, os.Getpid())
	}
}

func TestPIDRefusesAConnectionWithNoPeerCredentials(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	if _, err := peercred.PID(a); err == nil {
		t.Error("PID of a pipe = nil error, want a refusal")
	}
}
