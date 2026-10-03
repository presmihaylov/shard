package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"
)

// testReserve is a small reserve under a temp root, with its log kept for the test to read.
func testReserve(t *testing.T) (reserve, *strings.Builder) {
	t.Helper()

	out := &strings.Builder{}
	r := newReserve(t.TempDir(), log.New(out, "", 0))
	r.size = 1 << 20

	return r, out
}

func full() error { return fmt.Errorf("write the record: %w", syscall.ENOSPC) }

func TestEnsureWritesTheReserveOnceAtItsSize(t *testing.T) {
	r, _ := testReserve(t)

	if err := r.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first, err := os.Stat(r.path)
	if err != nil {
		t.Fatalf("stat the reserve: %v", err)
	}
	if first.Size() != r.size {
		t.Errorf("the reserve holds %d bytes, want %d", first.Size(), r.size)
	}

	if err := r.ensure(); err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	again, err := os.Stat(r.path)
	if err != nil {
		t.Fatalf("stat the reserve again: %v", err)
	}
	if !again.ModTime().Equal(first.ModTime()) {
		t.Errorf("a second start rewrote the reserve at %s, want it left from %s", again.ModTime(), first.ModTime())
	}
}

// A root already near full starts without a reserve, rather than take the last space for it.
func TestEnsureHoldsNoReserveOnAFullerRoot(t *testing.T) {
	r, out := testReserve(t)
	r.size = 1 << 55

	if err := r.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := os.Stat(r.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat the reserve: %v, want no reserve", err)
	}
	if !strings.Contains(out.String(), "holds none") {
		t.Errorf("the log says %q, want the line on the missing reserve", out.String())
	}
}

func TestRetryDeletesTheReserveAndRunsTheStepOnceMore(t *testing.T) {
	r, out := testReserve(t)
	if err := r.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	runs := 0
	err := r.retry("the socket bind", func() error {
		runs++
		if runs == 1 {
			return full()
		}

		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if runs != 2 {
		t.Errorf("the step ran %d times, want 2", runs)
	}
	if _, err := os.Stat(r.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat the reserve: %v, want it deleted", err)
	}
	if !strings.Contains(out.String(), "the socket bind found the root full") || !strings.Contains(out.String(), "bytes free before") {
		t.Errorf("the log says %q, want the step and the free space before and after", out.String())
	}
}

func TestRetryFailsOnASecondFullRoot(t *testing.T) {
	r, _ := testReserve(t)
	if err := r.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	runs := 0
	err := r.retry("the socket bind", func() error {
		runs++

		return full()
	})
	if !errors.Is(err, syscall.ENOSPC) || !strings.Contains(err.Error(), "the socket bind again") {
		t.Errorf("retry = %v, want the second ENOSPC, named for the step", err)
	}
	if runs != 2 {
		t.Errorf("the step ran %d times, want 2", runs)
	}
}

func TestRetryRunsOnceWithoutAReserve(t *testing.T) {
	r, out := testReserve(t)

	runs := 0
	err := r.retry("the socket bind", func() error {
		runs++

		return full()
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("retry = %v, want the ENOSPC of the step", err)
	}
	if runs != 1 {
		t.Errorf("the step ran %d times, want once: no reserve gives back room", runs)
	}
	if out.Len() != 0 {
		t.Errorf("the log says %q, want nothing: no reserve was deleted", out.String())
	}
}

func TestRetryKeepsTheReserveOnAnyOtherError(t *testing.T) {
	r, _ := testReserve(t)
	if err := r.ensure(); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	want := errors.New("permission denied")
	runs := 0
	err := r.retry("the socket bind", func() error {
		runs++

		return want
	})
	if !errors.Is(err, want) || runs != 1 {
		t.Errorf("retry = %v after %d runs, want the step's own error after one", err, runs)
	}
	if _, err := os.Stat(r.path); err != nil {
		t.Errorf("stat the reserve: %v, want it kept", err)
	}
}
