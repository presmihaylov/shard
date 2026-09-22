package image_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/image"
)

// fakeMkfsErofs puts a mkfs.erofs stand-in first on PATH that logs its argv to the returned path and writes a marker as the image.
func fakeMkfsErofs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"" + log + "\" && printf EROFS > \"$3\"\n"
	if err := os.WriteFile(filepath.Join(dir, "mkfs.erofs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

func TestPullWithErofsBuildsOneImagePerDigestFromTheTree(t *testing.T) {
	log := fakeMkfsErofs(t)
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server, image.WithErofs())

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Erofs == "" || filepath.Dir(img.Erofs) != filepath.Join(root, "erofs") || filepath.Ext(img.Erofs) != ".erofs" {
		t.Fatalf("Erofs is %q", img.Erofs)
	}
	body, err := os.ReadFile(img.Erofs)
	if err != nil {
		t.Fatalf("the image is not there: %v", err)
	}
	if string(body) != "EROFS" {
		t.Errorf("the image holds %q", body)
	}
	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(argv), img.RootFS+"\n") {
		t.Errorf("mkfs.erofs ran over:\n%s\nwant the tree %s", argv, img.RootFS)
	}
	if img.Disk != "" {
		t.Errorf("Disk is %q on a host with erofs only", img.Disk)
	}

	// An image that went missing under a cached pull is built again by the next one; the tree stays.
	if err := os.Remove(img.Erofs); err != nil {
		t.Fatalf("remove the image: %v", err)
	}
	if _, err := svc.Pull(t.Context(), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if _, err := os.Stat(img.Erofs); err != nil {
		t.Errorf("the image did not come back: %v", err)
	}
}

func TestPullWithErofsFailsWhenTheToolDoes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mkfs.erofs"), []byte("#!/bin/sh\necho 'no space' >&2; exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server, image.WithErofs())

	_, err := svc.Pull(t.Context(), ref)
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("Pull: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "erofs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the failed build left %v behind", entries)
	}
}

func TestRemoveWithErofsDropsTheImage(t *testing.T) {
	fakeMkfsErofs(t)
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	svc := newServiceAt(t, t.TempDir(), server, image.WithErofs())

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := svc.Remove(t.Context(), ref, nothing); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(img.Erofs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the image survived the removal: %v", err)
	}
}

func TestPullSweepsAStaleStagingErofsImage(t *testing.T) {
	fakeMkfsErofs(t)
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server, image.WithErofs())

	stale := filepath.Join(root, "erofs", ".unpack-123456")
	if err := os.WriteFile(stale, []byte("half"), 0o600); err != nil {
		t.Fatalf("plant the staging image: %v", err)
	}
	if _, err := svc.Pull(t.Context(), ref); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging image survived: %v", err)
	}
}

func TestPullWithoutErofsReportsNone(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	svc := newService(t, server)

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if img.Erofs != "" {
		t.Errorf("Erofs is %q on a host without them", img.Erofs)
	}
}
