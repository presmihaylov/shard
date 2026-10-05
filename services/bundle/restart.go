package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/filemode"
)

// restartFileCap bounds the read: shard-init writes a few dozen bytes, and the guest can write anything there.
const restartFileCap = 4 << 10

// RestartCount reads what shard-init kept of its restart policy on this run, zero before the first start again, off a disk a stop detached too (SHARD-401).
func (b Bundle) RestartCount() (models.RestartCount, error) {
	var count models.RestartCount
	err := b.withDisk(func() error {
		var err error
		count, err = readRestartCount(b.RestartFile)

		return err
	})
	if err != nil {
		return models.RestartCount{}, err
	}

	return count, nil
}

// readRestartCount takes only a small regular file, because the guest can write that path too.
func readRestartCount(path string) (models.RestartCount, error) {
	f, err := openRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return noRestartYet(path)
	}
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("open the restart count: %w", err)
	}
	defer f.Close()

	blob, err := io.ReadAll(io.LimitReader(f, restartFileCap+1))
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("read the restart count: %w", err)
	}
	if len(blob) > restartFileCap {
		return models.RestartCount{}, fmt.Errorf("the restart count %s is over %d bytes", path, restartFileCap)
	}

	var count models.RestartCount
	if err := json.Unmarshal(blob, &count); err != nil {
		return models.RestartCount{}, fmt.Errorf("decode the restart count: %w", err)
	}

	return count, nil
}

// noRestartYet reads a missing file as zero only where shard-init could have written it, so a disk not mounted is an error and never a zero.
func noRestartYet(path string) (models.RestartCount, error) {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return models.RestartCount{}, fmt.Errorf("the restart count %s cannot be read: %w", path, err)
	}

	return models.RestartCount{}, nil
}

// requireRegular refuses anything but a regular file, whose read can neither block nor reach a driver.
func requireRegular(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is a %s, and it must be a regular file", path, filemode.Name(info.Mode()))
	}

	return nil
}
