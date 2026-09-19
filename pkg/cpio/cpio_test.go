package cpio_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/pkg/cpio"
)

// The system cpio is the reference reader; the short flags are what BSD, GNU and busybox share.
func TestTheArchiveUnpacksWithTheSystemCpio(t *testing.T) {
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("no cpio on this host")
	}

	var buf bytes.Buffer
	w := cpio.New(&buf)
	body := bytes.Repeat([]byte("x"), 4097)
	if err := w.File("init", 0o755, body); err != nil {
		t.Fatal(err)
	}
	if err := w.File("etc/hostname", 0o644, []byte("guest\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cmd := exec.Command("cpio", "-i", "-d")
	cmd.Dir = dir
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cpio -i: %v: %s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(dir, "init"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("init: %v, %d bytes", err, len(got))
	}
	info, err := os.Stat(filepath.Join(dir, "init"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("init mode: %v %v", info.Mode(), err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "etc", "hostname")); err != nil || string(got) != "guest\n" {
		t.Fatalf("hostname: %q %v", got, err)
	}
}

func TestAWriteAfterAFailureReportsTheFailure(t *testing.T) {
	w := cpio.New(failing{})
	first := w.File("init", 0o755, nil)
	if first == nil {
		t.Fatal("a failing writer took the file")
	}
	if err := w.Close(); err == nil || err.Error() != first.Error() {
		t.Fatalf("close = %v, want the first failure again", err)
	}
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, os.ErrClosed }
