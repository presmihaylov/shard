package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/presmihaylov/shard/models"
)

// restartFileCap bounds the read: shard-init writes a few dozen bytes, and the guest can write anything there.
const restartFileCap = 4 << 10

// RestartCount reads what shard-init kept of its restart policy on this run: zero before the first start again.
func (b Bundle) RestartCount() (models.RestartCount, error) {
	f, err := openRegular(b.RestartFile)
	if errors.Is(err, os.ErrNotExist) {
		return models.RestartCount{}, nil
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
		return models.RestartCount{}, fmt.Errorf("the restart count %s is over %d bytes", b.RestartFile, restartFileCap)
	}

	var count models.RestartCount
	if err := json.Unmarshal(blob, &count); err != nil {
		return models.RestartCount{}, fmt.Errorf("decode the restart count: %w", err)
	}

	return count, nil
}

// requireRegular refuses anything but a regular file, whose read can neither block nor reach a driver.
func requireRegular(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is a %s, and it must be a regular file", path, info.Mode().Type())
	}

	return nil
}
