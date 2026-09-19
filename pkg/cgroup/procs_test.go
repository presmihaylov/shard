package cgroup_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
