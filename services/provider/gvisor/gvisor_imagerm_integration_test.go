//go:build integration

package gvisor_test

import (
	"context"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/image"
)

// SHARD-359: a create that pulls while image remove is past its check must still find its rootfs at the mount.
func TestACreateKeepsTheRootFSAnImageRmHasPassed(t *testing.T) {
	h := newHarness(t)

	// Its own image root, so the removal never takes the rootfs the rest of the package shares.
	svc, err := image.New(t.TempDir())
	if err != nil {
		t.Fatalf("open the image service: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := svc.Pull(ctx, testImage); err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}

	checked := make(chan struct{})
	release := make(chan struct{})
	removed := make(chan error, 1)
	go func() {
		removed <- svc.Remove(context.Background(), testImage, func() error {
			close(checked)
			<-release

			return nil
		})
	}()
	<-checked

	var img image.Image
	var pullErr error
	pulled := make(chan struct{})
	go func() {
		defer close(pulled)
		img, pullErr = svc.Pull(ctx, testImage)
	}()

	// A pull that waits for the removal never returns here, so the removal goes on after a beat either way.
	select {
	case <-pulled:
	case <-time.After(2 * time.Second):
	}
	close(release)

	if err := <-removed; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	<-pulled
	if pullErr != nil {
		t.Fatalf("Pull during the removal: %v", pullErr)
	}

	spec := h.newSpec(t)
	spec.RootFS = img.RootFS
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create over the rootfs the pull handed out: %v", err)
	}
}
