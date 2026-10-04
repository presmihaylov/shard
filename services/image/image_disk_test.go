package image_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/services/image"
)

func TestPullWithDisksBuildsOneDiskPerDigest(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server, image.WithDisks())

	progress := image.NewProgress()
	img, err := svc.Pull(image.WithProgress(t.Context(), progress), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	progress.Close()
	if img.Disk == "" || filepath.Dir(img.Disk) != filepath.Join(root, "disks") {
		t.Fatalf("Disk is %q", img.Disk)
	}
	if !slices.Contains(events(t, progress), image.Event{Status: image.StatusBuilding, Path: img.Disk}) {
		t.Errorf("the pull never said it was building the disk %s", img.Disk)
	}
	info, err := os.Stat(img.Disk)
	if err != nil {
		t.Fatalf("the disk is not there: %v", err)
	}
	if info.Size()%4096 != 0 || info.Size() == 0 {
		t.Errorf("the disk is %d bytes", info.Size())
	}

	// A disk that went missing under a cached image is built again by the next pull; the tree stays.
	if err := os.Remove(img.Disk); err != nil {
		t.Fatalf("remove the disk: %v", err)
	}
	again := image.NewProgress()
	if _, err := svc.Pull(image.WithProgress(t.Context(), again), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	again.Close()
	// The tree and the tag are already held, so the rebuild never unpacks (SHARD-385) and never asks the registry.
	want := []string{image.StatusBuilding, image.StatusPulled}
	if got := statuses(events(t, again)); !slices.Equal(got, want) {
		t.Errorf("the rebuild of the disk said %v, want %v", got, want)
	}
	if _, err := os.Stat(img.Disk); err != nil {
		t.Errorf("the disk did not come back: %v", err)
	}
}

func TestRemoveWithDisksDropsTheDisk(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	svc := newServiceAt(t, t.TempDir(), server, image.WithDisks())

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := svc.Remove(t.Context(), ref, nothing); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(img.Disk); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the disk survived the removal: %v", err)
	}
}

func TestPullSweepsAStaleStagingDisk(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server, image.WithDisks())

	stale := filepath.Join(root, "disks", ".unpack-123456")
	if err := os.WriteFile(stale, []byte("half"), 0o600); err != nil {
		t.Fatalf("plant the staging disk: %v", err)
	}
	if _, err := svc.Pull(t.Context(), ref); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging disk survived: %v", err)
	}
}

func TestPullWithoutDisksReportsNone(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	svc := newService(t, server)

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Disk != "" {
		t.Errorf("Disk is %q on a host without disks", img.Disk)
	}
}
