package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/presmihaylov/shard/models"
)

// restartFileCap bounds the read: shard-init writes a few dozen bytes, and the guest can write anything there.
const restartFileCap = 4 << 10

// RestartCount reads what shard-init kept of its restart policy on this run: zero before the first start again.
func (b Bundle) RestartCount() (models.RestartCount, error) {
	// The guest writes this directory, so a symlink would resolve against the host's root and a fifo would block the open (SHARD-305).
	f, err := os.OpenFile(b.RestartFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return models.RestartCount{}, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return models.RestartCount{}, fmt.Errorf("the restart count %s is a symbolic link, and it must be a regular file", b.RestartFile)
	}
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("open the restart count: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("stat the restart count: %w", err)
	}
	if !info.Mode().IsRegular() {
		return models.RestartCount{}, fmt.Errorf("the restart count %s is a %s, and it must be a regular file", b.RestartFile, info.Mode().Type())
	}

	blob, err := io.ReadAll(io.LimitReader(f, restartFileCap+1))
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("read the restart count: %w", err)
	}
	if len(blob) > restartFileCap {
		return models.RestartCount{}, fmt.Errorf("the restart count %s is over %d bytes", b.RestartFile, restartFileCap)
	}

	var count models.RestartCount
	if err := json.Unmarshal(blob, &count); err != nil {
		return models.RestartCount{}, fmt.Errorf("decode the restart count: %w", err)
	}

	return count, nil
}
