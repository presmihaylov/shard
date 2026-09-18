package bundle_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// TestAnUnboundedSandboxStillGetsTheDiskDefault pins that a disk bound, like pids, is never off: 0 takes the default.
func TestAnUnboundedSandboxStillGetsTheDiskDefault(t *testing.T) {
	for _, r := range []models.Resources{{}, {DiskMiB: 0}} {
		if got := bundle.DiskBound(r); got != bundle.DefaultDiskMiB {
			t.Errorf("DiskBound(%v) = %d, want the default %d", r, got, bundle.DefaultDiskMiB)
		}
	}
}

func TestADiskBoundIsTheNumberTheOperatorTyped(t *testing.T) {
	if got := bundle.DiskBound(models.Resources{DiskMiB: 64}); got != 64 {
		t.Errorf("DiskBound = %d, want 64", got)
	}
	if got := bundle.DiskBytes(models.Resources{DiskMiB: 64}); got != 64<<20 {
		t.Errorf("DiskBytes = %d, want 64 MiB", got)
	}
}

// One bound covers the overlay, /tmp and the supervisor's files together, so all of them sit on the disk.
func TestEverythingTheGuestWritesLivesOnTheDisk(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	for _, dir := range []string{b.Upper, b.Work, b.Tmp, b.ShardDir} {
		if !strings.HasPrefix(dir, b.Disk+string(filepath.Separator)) {
			t.Errorf("%s is off the disk %s", dir, b.Disk)
		}
	}
	// The exit record, config.json and the image itself stay on the host, so a full disk never hides how the guest ended.
	for _, path := range []string{b.ExitFile, b.Dir, b.Image} {
		if strings.HasPrefix(path, b.Disk+string(filepath.Separator)) {
			t.Errorf("%s is on the disk, want it on the host", path)
		}
	}
}

func TestBuildRecordsTheDiskBoundForTheNextStart(t *testing.T) {
	cases := map[string]struct{ typed, want int64 }{
		"a typed bound": {64, 64},
		"the default":   {0, bundle.DefaultDiskMiB},
	}

	for name, c := range cases {
		b, _ := build(t, models.SandboxSpec{Resources: models.Resources{DiskMiB: c.typed}}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

		rt, err := b.Runtime()
		if err != nil {
			t.Fatalf("%s: Runtime: %v", name, err)
		}
		if rt.Resources.DiskMiB != c.want {
			t.Errorf("%s: read back a disk bound of %d, want %d", name, rt.Resources.DiskMiB, c.want)
		}
	}
}

// A clone is bounded the way its source was: config.json carries the bound and the provider provisions from it.
func TestCloneCarriesTheDiskBound(t *testing.T) {
	source := newSpec(t)
	source.Resources = models.Resources{DiskMiB: 64}
	build(t, source, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	opened, err := bundle.Open(source.StateDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	clone := models.SandboxSpec{ID: "s-clone", StateDir: t.TempDir(), Network: models.NetworkSpec{NetnsPath: "/run/netns/s-clone"}}
	c, err := newService(t).Clone(opened, clone)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	rt, err := c.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rt.Resources.DiskMiB != 64 {
		t.Errorf("the clone read back a disk bound of %d, want the source's 64", rt.Resources.DiskMiB)
	}
}

func TestRuntimeRefusesADiskBoundThatIsNotACount(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{Resources: models.Resources{DiskMiB: 64}}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	config := filepath.Join(b.Dir, "config.json")
	broken := strings.Replace(readFile(t, config), `"dev.shard.disk-mib": "64"`, `"dev.shard.disk-mib": "lots"`, 1)
	if !strings.Contains(broken, "lots") {
		t.Fatal("config.json does not carry the disk bound annotation to break")
	}
	write(t, config, broken)

	if _, err := b.Runtime(); err == nil {
		t.Error("Runtime read back a disk bound that is not a count")
	}
}
