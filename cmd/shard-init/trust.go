package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/supervisor"
)

// writeTrustIn lays the merged CA bundle down under root by rename, at the path the image reads its roots from.
func writeTrustIn(root string, t supervisor.Trust) error {
	target := filepath.Join(root, filepath.FromSlash(t.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // every TLS client reads the store
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	if err := store.WriteFile(target, t.Roots, 0o644); err != nil { //nolint:gosec // every TLS client reads the store
		return fmt.Errorf("write the trust store %s: %w", t.Path, err)
	}

	return nil
}
