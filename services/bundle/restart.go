package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/filemode"
)

// restartFileCap bounds the read: the daemon writes a few dozen bytes there.
const restartFileCap = 4 << 10

// RestartCount reads the count shard-init keeps on its exit record, zero before the first start again (SHARD-634).
func (b Bundle) RestartCount() (models.RestartCount, error) {
	blob, err := readExitFile(b.ExitFile)
	if errors.Is(err, fs.ErrNotExist) {
		return models.RestartCount{}, nil
	}
	if err != nil {
		return models.RestartCount{}, err
	}

	report, _, err := decodeReport(blob, models.ExitReportKind)
	if err != nil {
		return models.RestartCount{}, fmt.Errorf("decode the restart count in %s: %w", b.ExitFile, err)
	}

	return report.Restarts, nil
}

// ClearRun drops what an earlier run left of the supervisor's files. The guest can leave a tree at ReadyFile, so a plain remove is not enough (SHARD-635).
func (b Bundle) ClearRun() error {
	for _, stale := range []string{b.ExitFile, b.ReadyFile, b.ChangedFile} {
		// The guest is down here, and RemoveAll follows no link, so nothing past the tree it left goes with it.
		if err := os.RemoveAll(stale); err != nil {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	return nil
}

// ReadRestartCount reads the count a VM provider keeps from the guest's events, and takes only a small regular file.
func ReadRestartCount(path string) (models.RestartCount, error) {
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
