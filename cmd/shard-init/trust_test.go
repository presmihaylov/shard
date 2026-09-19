package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

// The image ships its roots as a symlink into a store shard-init never mounts; the write replaces it with the merged bundle.
func TestTheTrustStoreReplacesTheImageRoots(t *testing.T) {
	root := t.TempDir()
	certs := filepath.Join(root, "etc/ssl/certs")
	if err := os.MkdirAll(certs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../usr/share/ca-certificates/mozilla.crt", filepath.Join(certs, "ca-certificates.crt")); err != nil {
		t.Fatal(err)
	}

	trust := supervisor.Trust{Path: "/etc/ssl/certs/ca-certificates.crt", Roots: []byte("roots\nproxy-ca\n")}
	if err := writeTrustIn(root, trust); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(certs, "ca-certificates.crt"))
	if err != nil || string(got) != "roots\nproxy-ca\n" {
		t.Fatalf("the trust store reads %q, %v; want the merged bundle", got, err)
	}
	if info, err := os.Lstat(filepath.Join(certs, "ca-certificates.crt")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the trust store is still a symlink: %v, %v", info, err)
	}
}
