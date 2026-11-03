package bundle_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/services/bundle"
)

func clone(from, to string) error {
	_, err := bundle.CloneFile(from, to)

	return err
}

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Size()
}

func TestGrowDiskGrowsTheDiskInPlace(t *testing.T) {
	disk := seedDisk(t)
	// A grow cut before its rename leaves the staged disk, which the next one clears.
	write(t, disk+".grow", "a cut grow")

	if err := bundle.GrowDisk(disk, models.Resources{DiskMiB: 128}, clone); err != nil {
		t.Fatalf("GrowDisk: %v", err)
	}

	if got := sizeOf(t, disk); got != 128<<20 {
		t.Errorf("the disk is %d bytes, want 128 MiB", got)
	}
	if _, err := os.Stat(disk + ".grow"); !os.IsNotExist(err) {
		t.Errorf("the staged disk stayed behind: %v", err)
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Logf("no e2fsck on PATH, the size stands alone")

		return
	}
	if out, err := exec.Command("e2fsck", "-fn", disk).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
}

func TestGrowDiskRefusesADirtyDiskAndLeavesItAsItWas(t *testing.T) {
	disk := seedDisk(t)
	markDirty(t, disk)

	err := bundle.GrowDisk(disk, models.Resources{DiskMiB: 128}, clone)
	if !errors.Is(err, ext4.ErrNeedsRecovery) {
		t.Fatalf("GrowDisk = %v, want ErrNeedsRecovery", err)
	}
	if !strings.Contains(err.Error(), "start the sandbox, stop it, and resize again") {
		t.Errorf("the refusal %q does not name the fix", err)
	}
	if got := sizeOf(t, disk); got != 64<<20 {
		t.Errorf("the refused disk is %d bytes, want the 64 MiB it was", got)
	}
	if _, err := os.Stat(disk + ".grow"); !os.IsNotExist(err) {
		t.Errorf("the staged disk stayed behind: %v", err)
	}
}

func TestResizeRewritesTheBoundsTheNextStartReads(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{Resources: models.Resources{MemoryMiB: 512, VCPUs: 1}}, models.ImageConfig{})
	want := models.Resources{MemoryMiB: 48, VCPUs: 2, DiskMiB: bundle.DefaultDiskMiB}

	if err := b.Resize(want); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	rt, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rt.Resources != want {
		t.Errorf("the next start runs under %+v, want %+v", rt.Resources, want)
	}
	var spec specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(b.Dir, "config.json"))), &spec); err != nil {
		t.Fatal(err)
	}
	if got := tmpfsMiB(t, spec, "/dev/shm"); got != 48 {
		t.Errorf("/dev/shm is %d MiB under a 48 MiB bound, want 48", got)
	}
}

// The bundle here was never provisioned, so the grow has no disk to reach and must leave the bound as it was.
func TestResizeAFailedGrowKeepsTheBound(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{Resources: models.Resources{DiskMiB: 64}}, models.ImageConfig{})

	if err := b.Resize(models.Resources{DiskMiB: 128}); err == nil {
		t.Fatal("Resize took a disk bound the disk does not have")
	}
	if got := diskBoundOf(t, b); got != 64 {
		t.Errorf("the refused resize left a %d MiB disk bound, want 64", got)
	}
}

func TestResizeRefusesADiskThatShrinks(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{Resources: models.Resources{DiskMiB: 128}}, models.ImageConfig{})

	err := b.Resize(models.Resources{DiskMiB: 64})
	if err == nil || !strings.Contains(err.Error(), "does not shrink") {
		t.Fatalf("Resize returned %v, want a refusal that says the disk does not shrink", err)
	}
	if got := diskBoundOf(t, b); got != 128 {
		t.Errorf("the refused shrink left a %d MiB disk bound, want 128", got)
	}
}

func diskBoundOf(t *testing.T, b bundle.Bundle) int64 {
	t.Helper()
	rt, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	return rt.Resources.DiskMiB
}
