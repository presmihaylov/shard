package image_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/services/image"
)

func TestRemoveRestoresTheArtifactsWhenTheIndexWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission fixture")
	}
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "cached"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server, image.WithDisks())
	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Join(root, "layout"))
	if err := svc.Remove(t.Context(), ref, nothing); !errors.Is(err, os.ErrPermission) || errors.Is(err, image.ErrNotReclaimed) {
		t.Fatalf("Remove returned %v, want an index write refusal", err)
	}
	assertCachedRemoval(t, svc, ref, img)
	reopened := newServiceAt(t, root, server, image.WithDisks())
	assertCachedRemoval(t, reopened, ref, img)
}

func TestRemoveRestoresTheRootFSWhenADiskCannotBeStaged(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "cached"})
	svc := newServiceAt(t, t.TempDir(), server, image.WithDisks())
	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(filepath.Dir(img.Disk), ".unpack-rm-"+filepath.Base(img.Disk))
	if err := os.Mkdir(staged, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "blocker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Remove(t.Context(), ref, nothing); err == nil || errors.Is(err, image.ErrNotReclaimed) {
		t.Fatalf("Remove returned %v, want an artifact stage refusal", err)
	}
	assertCachedRemoval(t, svc, ref, img)
	data, err := os.ReadFile(filepath.Join(staged, "blocker"))
	if err != nil || string(data) != "keep" {
		t.Fatalf("the failed rename changed its destination: %q, %v", data, err)
	}
}

func TestRemoveDoesNotRestoreAnImageAfterACollectFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission fixture")
	}
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "cached"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server)
	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Join(root, "layout", "blobs", "sha256"))
	if err := svc.Remove(t.Context(), ref, nothing); !errors.Is(err, image.ErrNotReclaimed) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Remove returned %v, want a collect failure after removal", err)
	}
	if _, found, err := svc.Lookup(ref); err != nil || found {
		t.Fatalf("Lookup after removal returned found=%v, err=%v", found, err)
	}
	images, err := svc.List()
	if err != nil || len(images) != 0 {
		t.Fatalf("List after removal returned %+v, %v", images, err)
	}
	if _, err := os.Stat(img.RootFS); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rootfs survived a completed removal: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(img.RootFS))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the completed removal left artifacts: %v, %v", entries, err)
	}
}

func readOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Error(err)
		}
	})
}

func assertCachedRemoval(t *testing.T, svc *image.Service, ref string, want image.Image) {
	t.Helper()
	img, found, err := svc.Lookup(ref)
	if err != nil || !found || img.Digest != want.Digest {
		t.Fatalf("the failed removal changed the cache: %+v, found=%v, err=%v", img, found, err)
	}
	images, err := svc.List()
	if err != nil || len(images) != 1 || images[0].Digest != want.Digest || images[0].Broken != "" {
		t.Fatalf("the failed removal changed the index: %+v, %v", images, err)
	}
	data, err := os.ReadFile(filepath.Join(want.RootFS, "etc", "hostname"))
	if err != nil || string(data) != "cached" {
		t.Fatalf("the failed removal changed the rootfs: %q, %v", data, err)
	}
	if _, err := os.Stat(want.Disk); err != nil {
		t.Fatalf("the failed removal lost the disk: %v", err)
	}
}
