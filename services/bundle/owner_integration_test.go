//go:build integration

package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteLayerShiftsWhatItMakesIntoTheNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("a chown into the namespace's ids needs root")
	}

	layer := t.TempDir()
	// The guest made /etc, so it holds a host id already; the image shipped /etc/ssl as guest uid 100, which a copy-up keeps raw.
	mkdirOwned(t, filepath.Join(layer, "etc"), 165541)
	mkdirOwned(t, filepath.Join(layer, "etc", "ssl"), 100)

	if err := writeLayer(layer, filepath.Join("etc", "ssl", "certs", "ca-certificates.crt"), []byte("proxy CA\n"), sysboxShift); err != nil {
		t.Fatalf("writeLayer: %v", err)
	}

	want := map[string]uint32{
		"etc":                               165541,
		"etc/ssl":                           165636,
		"etc/ssl/certs":                     165536,
		"etc/ssl/certs/ca-certificates.crt": 165536,
	}
	for name, id := range want {
		info, err := os.Lstat(filepath.Join(layer, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		uid, gid, err := owner(info)
		if err != nil {
			t.Fatalf("owner of %s: %v", name, err)
		}
		if uid != id || gid != id {
			t.Errorf("%s is owned by %d:%d, want %d:%d", name, uid, gid, id, id)
		}
	}
}

func mkdirOwned(t *testing.T, dir string, id int) {
	t.Helper()

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Chown(dir, id, id); err != nil {
		t.Fatalf("chown %s: %v", dir, err)
	}
}
