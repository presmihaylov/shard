package cgroup_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/cgroup"
)

func procs(t *testing.T, dir, contents string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make the cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write cgroup.procs: %v", err)
	}
}

func TestProcsListsTheCgroupAndEveryOneUnderIt(t *testing.T) {
	dir := t.TempDir()
	procs(t, dir, "1101\n1102\n")
	procs(t, filepath.Join(dir, "leaf"), "1103\n")

	got, err := cgroup.Procs(dir)
	if err != nil {
		t.Fatalf("Procs: %v", err)
	}
	if want := []int{1101, 1102, 1103}; !slices.Equal(got, want) {
		t.Fatalf("Procs = %v, want %v", got, want)
	}
}

func TestProcsOfAnEmptyCgroupIsEmpty(t *testing.T) {
	dir := t.TempDir()
	procs(t, dir, "")

	got, err := cgroup.Procs(dir)
	if err != nil {
		t.Fatalf("Procs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Procs = %v, want none", got)
	}
}

func TestProcsOfACgroupThatIsGoneIsNamedAsSuch(t *testing.T) {
	_, err := cgroup.Procs(filepath.Join(t.TempDir(), "nothing"))
	if !errors.Is(err, cgroup.ErrNotFound) {
		t.Fatalf("Procs on a missing cgroup = %v, want ErrNotFound", err)
	}
}

func TestProcsRefusesAnEntryThatIsNotAPid(t *testing.T) {
	dir := t.TempDir()
	procs(t, dir, "1101\nsentry\n")

	_, err := cgroup.Procs(dir)
	if err == nil || !strings.Contains(err.Error(), `"sentry" is not a pid`) {
		t.Fatalf("Procs = %v, want the entry named", err)
	}
}

// removedMidRead makes dir's cgroup.procs a fifo that answers only once remove is gone, so the walk meets the removal at a known point.
func removedMidRead(t *testing.T, dir, contents, remove string) <-chan error {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make the cgroup: %v", err)
	}
	fifo := filepath.Join(dir, "cgroup.procs")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("make the fifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		if err := os.RemoveAll(remove); err != nil {
			done <- errors.Join(err, w.Close())
			return
		}
		_, err = w.WriteString(contents)
		done <- errors.Join(err, w.Close())
	}()

	return done
}

func TestProcsSkipsAChildTheGuestRemovesBeforeItIsRead(t *testing.T) {
	dir := t.TempDir()
	procs(t, dir, "1101\n")
	procs(t, filepath.Join(dir, "c2"), "1103\n")
	done := removedMidRead(t, filepath.Join(dir, "c1"), "1102\n", filepath.Join(dir, "c2"))

	got, err := cgroup.Procs(dir)
	if err != nil {
		t.Fatalf("Procs with a child gone mid-walk: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("remove the child: %v", err)
	}
	if want := []int{1101, 1102}; !slices.Equal(got, want) {
		t.Fatalf("Procs = %v, want %v", got, want)
	}
}

func TestProcsSkipsAChildTheGuestRemovesAfterItsProcsAreRead(t *testing.T) {
	dir := t.TempDir()
	procs(t, dir, "1101\n")
	child := filepath.Join(dir, "c1")
	done := removedMidRead(t, child, "1102\n", child)

	got, err := cgroup.Procs(dir)
	if err != nil {
		t.Fatalf("Procs with a child gone before its own children were listed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("remove the child: %v", err)
	}
	if want := []int{1101, 1102}; !slices.Equal(got, want) {
		t.Fatalf("Procs = %v, want %v", got, want)
	}
}

func TestProcsNeverReadsAChildMadeAgainAsCgroupV1(t *testing.T) {
	dir := t.TempDir()
	procs(t, dir, "1101\n")
	if err := os.Mkdir(filepath.Join(dir, "c1"), 0o755); err != nil {
		t.Fatalf("make the child: %v", err)
	}

	got, err := cgroup.Procs(dir)
	if err != nil {
		t.Fatalf("Procs with a child that has no cgroup.procs yet: %v", err)
	}
	if want := []int{1101}; !slices.Equal(got, want) {
		t.Fatalf("Procs = %v, want %v", got, want)
	}
}

func TestProcsOfACgroupWithoutItsProcsIsNoController(t *testing.T) {
	_, err := cgroup.Procs(t.TempDir())
	if !errors.Is(err, cgroup.ErrNoController) {
		t.Fatalf("Procs on a cgroup with no cgroup.procs = %v, want ErrNoController", err)
	}
}
