package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/bundle"
)

// resolveBudget bounds a lookup that must answer, so a database that blocks fails rather than hangs.
const resolveBudget = 5 * time.Second

// An exec resolves against the sandbox's live tree, and a guest with root in it writes what it likes.
func TestResolveUserRefusesAPasswdThatIsASymbolicLink(t *testing.T) {
	rootfs := emptyRootFS(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(rootfs, "etc/passwd")); err != nil {
		t.Fatalf("link the passwd file: %v", err)
	}

	_, _, err := bundle.ResolveUser(rootfs, "root")
	if err == nil {
		t.Fatal("ResolveUser read a passwd file that points out of the rootfs")
	}
	if !strings.Contains(err.Error(), filepath.Join(rootfs, "etc/passwd")) {
		t.Errorf("the refusal is %q, and it must name the file", err)
	}
}

// A fifo answers only when the guest writes to it, so an unbounded open is a hang the operator cannot end.
func TestResolveUserRefusesAPasswdThatIsAFifo(t *testing.T) {
	rootfs := emptyRootFS(t)
	if err := syscall.Mkfifo(filepath.Join(rootfs, "etc/passwd"), 0o600); err != nil {
		t.Fatalf("make the passwd fifo: %v", err)
	}

	failed := make(chan error, 1)
	go func() {
		_, _, err := bundle.ResolveUser(rootfs, "root")
		failed <- err
	}()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("ResolveUser read a passwd file that is a fifo")
		}
		if !strings.Contains(err.Error(), "regular file") {
			t.Errorf("the refusal is %q, and it must say what the file must be", err)
		}
	case <-time.After(resolveBudget):
		t.Fatalf("ResolveUser did not answer within %s, so it is waiting on the fifo", resolveBudget)
	}
}

func emptyRootFS(t *testing.T) string {
	t.Helper()

	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("create the rootfs: %v", err)
	}

	return rootfs
}
