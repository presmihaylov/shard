package image

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/presmihaylov/shard/pkg/store"
)

type removalArtifact struct {
	path   string
	staged string
}

func (s *Service) stageRemoval(digests []string) ([]removalArtifact, error) {
	var moved []removalArtifact
	for _, digest := range digests {
		staged, err := s.stageDigestRemoval(digest)
		moved = append(moved, staged...)
		if err != nil {
			return moved, err
		}
	}

	return moved, nil
}

func (s *Service) stageDigestRemoval(digest string) ([]removalArtifact, error) {
	var moved []removalArtifact
	for _, path := range []string{s.rootfsDir(digest), s.diskPath(digest), s.erofsPath(digest)} {
		staged := filepath.Join(filepath.Dir(path), stagingPrefix+"rm-"+filepath.Base(path))
		err := os.Rename(path, staged)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return moved, fmt.Errorf("stage %s for removal: %w", path, err)
		}
		moved = append(moved, removalArtifact{path: path, staged: staged})
	}

	return moved, nil
}

func restoreRemoval(moved []removalArtifact) error {
	var restored error
	for _, artifact := range slices.Backward(moved) {
		if err := os.Rename(artifact.staged, artifact.path); err != nil {
			restored = errors.Join(restored, fmt.Errorf("restore %s after the removal failed: %w", artifact.path, err))
			continue
		}
		restored = errors.Join(restored, store.SyncDir(filepath.Dir(artifact.path)))
	}

	return restored
}
