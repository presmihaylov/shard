package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// canonicalAlpine is the reference a record holds: the create wrote it in its canonical form.
const canonicalAlpine = "index.docker.io/library/alpine:3.20"

// snapshotSource is a stopped sandbox over an image the host still holds, which is what a snapshot copies.
func snapshotSource() models.Sandbox {
	sb := stopped()
	sb.Image = canonicalAlpine
	sb.Digest = fakeDigest
	sb.Resources = models.Resources{MemoryMiB: 512, DiskMiB: 2048}

	return sb
}

// storedSnapshot writes a snapshot into the store directly, so a create test needs no source sandbox.
func storedSnapshot(t *testing.T, l layers, snap models.Snapshot) models.Snapshot {
	t.Helper()

	made, err := l.snapshots.Create(snap, func(files string) error {
		return os.WriteFile(filepath.Join(files, "upper"), []byte("kept"), 0o600)
	})
	if err != nil {
		t.Fatalf("store the snapshot: %v", err)
	}

	return made
}

func baseSnapshot() models.Snapshot {
	return models.Snapshot{Name: "base", Source: "sandbox9", Image: canonicalAlpine, Digest: fakeDigest, Provider: "fake", DiskMiB: 2048, MemoryMiB: 512}
}

func TestCreateSnapshotCopiesAStoppedSandbox(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, snapshotSource())

	snap, err := svc.CreateSnapshot(t.Context(), sandbox.SnapshotRequest{Sandbox: "web", Name: "base"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	want := models.Snapshot{ID: snap.ID, Name: "base", Source: "sandbox1", SourceName: "web", Image: canonicalAlpine, Digest: fakeDigest, Provider: "fake", DiskMiB: 2048, MemoryMiB: 512}
	if snap.ID == "" || snap.CreatedAt.IsZero() || snap.Size == 0 {
		t.Errorf("the snapshot is %+v, want an id, a time and the size of what it holds", snap)
	}
	snap.Size, snap.CreatedAt = 0, want.CreatedAt
	if snap != want {
		t.Errorf("the snapshot is %+v, want %+v", snap, want)
	}
	if l.provider.source != "sandbox1" {
		t.Errorf("the provider copied %q, want the source sandbox1", l.provider.source)
	}

	byName, err := svc.InspectSnapshot(t.Context(), "base")
	if err != nil || byName.ID != snap.ID {
		t.Errorf("inspect by name answered %+v and %v, want the snapshot %s", byName, err, snap.ID)
	}
	all, err := svc.ListSnapshots(t.Context())
	if err != nil || len(all) != 1 || all[0].ID != snap.ID {
		t.Errorf("list answered %+v and %v, want the one snapshot %s", all, err, snap.ID)
	}
}

// Only a stopped sandbox holds still: a live one writes the layer under the copy, and a paused one holds its run in a checkpoint.
func TestCreateSnapshotRefusesASandboxThatIsNotStopped(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StatePaused, models.StateCreated} {
		r := &recorder{}
		source := snapshotSource()
		source.State = state
		svc, l := newService(t, r, source)

		_, err := svc.CreateSnapshot(t.Context(), sandbox.SnapshotRequest{Sandbox: "web"})

		var refused *sandbox.StateError
		if !errors.As(err, &refused) || refused.Code != models.CodeSandboxNotStopped || !strings.Contains(err.Error(), "shard stop web") {
			t.Errorf("a snapshot of a %s sandbox returned %v, want sandbox_not_stopped and the stop that fixes it", state, err)
		}
		if slices.Contains(r.calls, "provider.Snapshot") {
			t.Errorf("a snapshot of a %s sandbox reached the provider", state)
		}
		if all, err := l.snapshots.List(); err != nil || len(all) != 0 {
			t.Errorf("a refused snapshot left %+v and %v, want nothing", all, err)
		}
	}
}

func TestCreateSnapshotRefusesASandboxWhoseImageIsGone(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, snapshotSource(), func(cfg *sandbox.Config) { cfg.Images = fakeImages{r: r, gone: true} })

	_, err := svc.CreateSnapshot(t.Context(), sandbox.SnapshotRequest{Sandbox: "web"})

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "shard pull "+canonicalAlpine) {
		t.Errorf("a snapshot over a gone image returned %v, want a refusal that names the pull", err)
	}
	if slices.Contains(r.calls, "provider.Snapshot") {
		t.Error("a snapshot over a gone image reached the provider")
	}
}

// The source's layer sits over the image it ran on, so a tag that moved since then would seed every create over another base.
func TestCreateSnapshotRefusesATagThatMovedSinceTheSourceRan(t *testing.T) {
	r := &recorder{}
	source := snapshotSource()
	source.Digest = "sha256:ffee"
	svc, l := newService(t, r, source)

	_, err := svc.CreateSnapshot(t.Context(), sandbox.SnapshotRequest{Sandbox: "web"})

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "now holds that tag at "+fakeDigest) {
		t.Errorf("a snapshot over a moved tag returned %v, want a refusal that names both digests", err)
	}
	if all, err := l.snapshots.List(); err != nil || len(all) != 0 {
		t.Errorf("a refused snapshot left %+v and %v, want nothing", all, err)
	}
}

func TestCreateSnapshotRefusesANameAnotherSnapshotHolds(t *testing.T) {
	svc, l := newService(t, &recorder{}, snapshotSource())
	held := storedSnapshot(t, l, baseSnapshot())

	_, err := svc.CreateSnapshot(t.Context(), sandbox.SnapshotRequest{Sandbox: "web", Name: "base"})

	var taken *sandboxstate.NameTakenError
	if !errors.As(err, &taken) || taken.Holder != held.ID {
		t.Errorf("a snapshot under a held name returned %v, want the name taken by %s", err, held.ID)
	}
}

// A sandbox from a snapshot starts over the copied layer, under the image the layer sits over, and never pulls.
func TestCreateFromASnapshotSeedsTheLayerAndNeverPulls(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	snap := storedSnapshot(t, l, baseSnapshot())

	sb, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if sb.Snapshot != snap.ID || sb.Image != canonicalAlpine || sb.Digest != fakeDigest || sb.Resources.DiskMiB != 2048 || sb.Resources.MemoryMiB != 512 {
		t.Errorf("the record holds snapshot %q, image %q at %q, disk %d and memory %d, want %s, %s at %s and the snapshot's 2048 and 512", sb.Snapshot, sb.Image, sb.Digest, sb.Resources.DiskMiB, sb.Resources.MemoryMiB, snap.ID, canonicalAlpine, fakeDigest)
	}
	files, err := l.snapshots.Files(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.provider.spec.Seed != files {
		t.Errorf("the provider was seeded from %q, want the snapshot's files %s", l.provider.spec.Seed, files)
	}
	if len(l.provider.spec.Entrypoint) != 0 {
		t.Errorf("the sandbox runs %v, want shard-init alone", l.provider.spec.Entrypoint)
	}
	if slices.Contains(r.calls, "images.Pull") || !slices.Contains(r.calls, "images.Lookup") {
		t.Errorf("the calls were %v, want a lookup and no pull", r.calls)
	}
}

func TestCreateRefusesARequestThatNamesAnImageAndASnapshot(t *testing.T) {
	cases := map[string]sandbox.CreateRequest{
		"both":    {Image: "alpine:3.20", Snapshot: "base"},
		"neither": {},
		"command": {Snapshot: "base", Command: []string{"echo", "1"}},
		"restart": {Snapshot: "base", Restart: &models.RestartSpec{Policy: models.RestartOnFailure}},
	}
	for name, req := range cases {
		r := &recorder{}
		svc, l := newService(t, r, models.Sandbox{})
		storedSnapshot(t, l, baseSnapshot())

		_, err := svc.Create(t.Context(), req)

		var refused *sandbox.RequestError
		if !errors.As(err, &refused) {
			t.Errorf("%s: create returned %v, want a request error", name, err)
		}
		if slices.Contains(r.calls, "repo.Create") {
			t.Errorf("%s: a refused create wrote a record", name)
		}
	}
}

func TestCreateFromASnapshotRefusesWhatTheSnapshotCannotStartOn(t *testing.T) {
	moved := baseSnapshot()
	moved.Digest = "sha256:ffee"
	foreign := baseSnapshot()
	foreign.Provider = "gvisor"

	cases := []struct {
		name string
		snap models.Snapshot
		gone bool
		want string
	}{
		{"another provider", foreign, false, "made on provider gvisor, and this server runs fake; create from it on a server that runs gvisor"},
		{"a moved tag", moved, false, "now holds that tag at " + fakeDigest},
		{"a gone image", baseSnapshot(), true, "never pulls"},
	}
	for _, c := range cases {
		r := &recorder{}
		svc, l := newService(t, r, models.Sandbox{}, func(cfg *sandbox.Config) { cfg.Images = fakeImages{r: r, gone: c.gone} })
		storedSnapshot(t, l, c.snap)

		_, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base"})

		var refused *sandbox.RequestError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: create returned %v, want a request error that says %q", c.name, err, c.want)
		}
		if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "images.Pull") {
			t.Errorf("%s: a refused create reached the store: %v", c.name, r.calls)
		}
	}
}

// Memory is not on the disk, so an explicit --memory replaces the snapshot's bound, and an explicit 0 is no bound.
func TestCreateFromASnapshotTakesAnExplicitMemory(t *testing.T) {
	for _, memory := range []int64{1024, 0} {
		r := &recorder{}
		svc, l := newService(t, r, models.Sandbox{})
		storedSnapshot(t, l, baseSnapshot())

		sb, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base", Resources: sandbox.ResourceRequest{MemoryMiB: new(memory)}})
		if err != nil {
			t.Fatalf("create with memory %d: %v", memory, err)
		}
		if sb.Resources.MemoryMiB != memory {
			t.Errorf("the record holds memory %d, want the request's %d over the snapshot's 512", sb.Resources.MemoryMiB, memory)
		}
	}
}

// A snapshot's bound is what its source ran with, so a VM's default never replaces it.
func TestCreateFromASnapshotOnAVMKeepsTheSnapshotMemory(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{}, withMemory(&memoryProvider{}))
	snap := baseSnapshot()
	snap.MemoryMiB = 1024
	storedSnapshot(t, l, snap)

	sb, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.Resources.MemoryMiB != 1024 || l.provider.spec.Resources.MemoryMiB != 1024 {
		t.Errorf("the record holds memory %d and the guest ran with %d, want the snapshot's 1024", sb.Resources.MemoryMiB, l.provider.spec.Resources.MemoryMiB)
	}
}

// A microVM grows its copy of the snapshot's disk, so a create there takes a larger --disk and refuses a smaller one.
func TestCreateFromASnapshotOnAMicroVMOnlyGrowsTheDisk(t *testing.T) {
	r := &recorder{}
	disks := &diskProvider{}
	svc, l := newService(t, r, models.Sandbox{}, withDisks(disks))
	storedSnapshot(t, l, baseSnapshot())

	_, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base", Resources: sandbox.ResourceRequest{DiskMiB: 1024}})

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "a disk only grows") || !strings.Contains(err.Error(), "2048 MiB or more") {
		t.Fatalf("create returned %v, want a refusal that names the disk that works", err)
	}
	if len(disks.admitted) != 0 || slices.Contains(r.calls, "repo.Create") {
		t.Errorf("a refused create admitted %v and made the calls %v", disks.admitted, r.calls)
	}

	for _, mib := range []int64{2048, 4096} {
		sb, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base", Resources: sandbox.ResourceRequest{DiskMiB: mib}})
		if err != nil {
			t.Fatalf("a create with --disk %dMiB returned %v", mib, err)
		}
		if sb.Resources.DiskMiB != mib {
			t.Errorf("the record holds disk %d, want %d", sb.Resources.DiskMiB, mib)
		}
	}
}

// A disk smaller than the snapshot's files fails late inside the copy, so the create refuses it up front, and takes a smaller disk that holds them (SHARD-583).
func TestCreateFromASnapshotRefusesADiskSmallerThanItsFiles(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	snap, err := l.snapshots.Create(baseSnapshot(), func(files string) error {
		return os.WriteFile(filepath.Join(files, "upper"), make([]byte, 3<<20), 0o600)
	})
	if err != nil {
		t.Fatalf("store the snapshot: %v", err)
	}

	_, err = svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base", Resources: sandbox.ResourceRequest{DiskMiB: 1}})

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "resources.disk_mib is 1 MiB") || !strings.Contains(err.Error(), "MiB or more") {
		t.Fatalf("a 1 MiB --disk over %d bytes of files returned %v, want a refusal that names the disk that works", snap.Size, err)
	}
	if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "provider.Create") {
		t.Errorf("a refused create made the calls %v", r.calls)
	}

	sb, err := svc.Create(t.Context(), sandbox.CreateRequest{Snapshot: "base", Resources: sandbox.ResourceRequest{DiskMiB: 8}})
	if err != nil {
		t.Fatalf("an 8 MiB --disk over %d bytes of files returned %v, want it taken below the snapshot's 2048", snap.Size, err)
	}
	if sb.Resources.DiskMiB != 8 {
		t.Errorf("the record holds disk %d, want 8", sb.Resources.DiskMiB)
	}
}

// The record pins the snapshot by id, so the background half reads the same one and fails the record when it went.
func TestCompleteFailsTheRecordWhenTheSnapshotWentAfterPrepare(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	storedSnapshot(t, l, baseSnapshot())
	req := sandbox.CreateRequest{Snapshot: "base"}

	sb, err := svc.Prepare(t.Context(), req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := svc.RemoveSnapshot(t.Context(), "base"); err != nil {
		t.Fatalf("remove the snapshot: %v", err)
	}

	if err := svc.Complete(t.Context(), sb.ID, req); !errors.Is(err, sandboxstate.ErrSnapshotNotFound) {
		t.Errorf("complete returned %v, want the snapshot not found", err)
	}
	if l.repo.sb.State != models.StateFailed || slices.Contains(r.calls, "provider.Create") {
		t.Errorf("the record is %s after the calls %v, want failed before the provider", l.repo.sb.State, r.calls)
	}
}

func TestRemoveSnapshotLeavesNothingToInspect(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})
	snap := storedSnapshot(t, l, baseSnapshot())

	if err := svc.RemoveSnapshot(t.Context(), snap.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, err := svc.InspectSnapshot(t.Context(), "base"); !errors.Is(err, sandboxstate.ErrSnapshotNotFound) {
		t.Errorf("inspect after the remove returned %v, want the snapshot not found", err)
	}
	if err := svc.RemoveSnapshot(t.Context(), snap.ID); !errors.Is(err, sandboxstate.ErrSnapshotNotFound) {
		t.Errorf("a second remove returned %v, want the snapshot not found", err)
	}
}
