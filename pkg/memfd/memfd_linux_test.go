//go:build linux

package memfd_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/memfd"
)

const size = 4096

func create(t *testing.T) *os.File {
	t.Helper()

	f, err := memfd.Create("memfd-test", size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})

	return f
}

// Every route a holder of the fd has to a bigger or smaller file ends in EPERM, a reopen through /proc included.
func TestNoFdChangesTheSizeOfAFixedMemfd(t *testing.T) {
	f := create(t)

	reopened, err := os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})

	attempts := map[string]func() error{
		"write past the end": func() error { _, err := f.WriteAt([]byte{'x'}, size); return err },
		"append":             func() error { _, err := reopened.Write([]byte{'x'}); return err },
		"grow":               func() error { return f.Truncate(2 * size) },
		"shrink":             func() error { return f.Truncate(0) },
		"fallocate":          func() error { return unix.Fallocate(int(f.Fd()), 0, 0, 2*size) },
		"seal writes":        func() error { _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE); return err },
	}
	for name, attempt := range attempts {
		if err := attempt(); !errors.Is(err, unix.EPERM) {
			t.Errorf("%s: got %v, want EPERM", name, err)
		}
	}

	if _, err := f.WriteAt([]byte("in place"), 0); err != nil {
		t.Fatalf("a write inside the size was refused: %v", err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Fatalf("the memfd is %d bytes, want %d", info.Size(), size)
	}
}

func TestFixedTellsTheMemfdFromAFileAndAPipe(t *testing.T) {
	fixed, err := memfd.Fixed(create(t))
	if err != nil || !fixed {
		t.Fatalf("Fixed on a created memfd: got %v, %v, want true", fixed, err)
	}

	plain, err := os.Create(filepath.Join(t.TempDir(), "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := plain.Close(); err != nil {
			t.Error(err)
		}
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(r.Close(), w.Close()); err != nil {
			t.Error(err)
		}
	})

	for _, f := range []*os.File{plain, r} {
		fixed, err := memfd.Fixed(f)
		if err != nil || fixed {
			t.Errorf("Fixed on %s: got %v, %v, want false", f.Name(), fixed, err)
		}
	}
}
