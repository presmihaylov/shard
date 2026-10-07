//go:build linux

package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// seededOverImageRoots is a seed over an image that holds the roots, whose upper etc/ssl/certs the caller then marks.
func seededOverImageRoots(t *testing.T) models.SandboxSpec {
	t.Helper()

	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), imageRoots)
	spec.Seed = seedWith(t, map[string]string{"marker": "seeded\n"})
	if err := os.MkdirAll(filepath.Join(spec.Seed, "upper/etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec.ProxyCA = []byte(proxyCA)

	return spec
}

// A file removed before the snapshot is gone from the guest, though the image below still holds it.
func TestBuildFindsNoRootsTheSeedWhitedOut(t *testing.T) {
	spec := seededOverImageRoots(t)
	if err := unix.Mknod(filepath.Join(spec.Seed, "upper/etc/ssl/certs/ca-certificates.crt"), unix.S_IFCHR, 0); err != nil {
		t.Skipf("this host makes no whiteout: %v", err)
	}

	if _, err := newService(t).Build(runspec.Resolve(spec, models.ImageConfig{})); !errors.Is(err, bundle.ErrNoCABundle) {
		t.Errorf("Build = %v, want %v", err, bundle.ErrNoCABundle)
	}
}

// A directory removed and made again before the snapshot hides every copy of it below.
func TestBuildFindsNoRootsUnderAnOpaqueSeedDirectory(t *testing.T) {
	spec := seededOverImageRoots(t)
	if err := unix.Setxattr(filepath.Join(spec.Seed, "upper/etc/ssl/certs"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("this host sets no trusted xattr: %v", err)
	}

	if _, err := newService(t).Build(runspec.Resolve(spec, models.ImageConfig{})); !errors.Is(err, bundle.ErrNoCABundle) {
		t.Errorf("Build = %v, want %v", err, bundle.ErrNoCABundle)
	}
}
