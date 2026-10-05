//go:build integration

package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrustProxyOnAMountedOverlayLeavesTheStoreRemovable is SHARD-526: a plant the merged view never saw made every guest unlink under /etc/ssl fail.
func TestTrustProxyOnAMountedOverlayLeavesTheStoreRemovable(t *testing.T) {
	requireRunsc(t)

	b, lower := buildBundle(t, t.TempDir(), []string{"/bin/true"})
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	// The lookup caches /etc/ssl with no upper directory, which is the state a created sandbox on runc and sysbox is in.
	if _, err := os.Stat(filepath.Join(b.RootFS, "etc/ssl/certs")); err != nil {
		t.Fatalf("look up the trust store through the merged view: %v", err)
	}

	const ca = "shard-526 proxy CA\n"
	if err := b.TrustProxy([]byte(ca)); err != nil {
		t.Fatalf("TrustProxy: %v", err)
	}

	if got := readFile(t, filepath.Join(b.RootFS, "etc/ssl/certs/ca-certificates.crt")); !strings.HasSuffix(got, ca) {
		t.Errorf("the merged view reads a trust store that does not end in the proxy CA")
	}

	if err := os.RemoveAll(filepath.Join(b.RootFS, "etc/ssl")); err != nil {
		t.Errorf("remove the trust store through the merged view, as the guest does: %v", err)
	}
}
