package bundle

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// heldLogEntries bounds one listing of Logs, whose entries guest root makes, so a flood of files costs the daemon one short read.
const heldLogEntries = 1024

// ProcessLog is where shard-init appends the output of the process name, refused for a name that is no valid one.
func (b Bundle) ProcessLog(name string) (string, error) {
	if !models.ValidProcessName(name) {
		return "", fmt.Errorf("%q is no valid process name", name)
	}

	return filepath.Join(b.Logs, supervisor.ProcessLogName(name)), nil
}

// ProcessLogs names each process log on a mounted disk, and none while the disk is down.
func (b Bundle) ProcessLogs() (_ []string, err error) {
	dir, err := os.Open(b.Logs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", b.Logs, err)
	}
	defer func() { err = errors.Join(err, dir.Close()) }()

	entries, err := dir.ReadDir(heldLogEntries)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("list %s: %w", b.Logs, err)
	}

	var paths []string
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		// A link or a stray file is a guest write, never a log shard-init opened.
		if supervisor.ProcessLogName(name) != entry.Name() || !entry.Type().IsRegular() || !models.ValidProcessName(name) {
			continue
		}
		paths = append(paths, filepath.Join(b.Logs, entry.Name()))
	}

	return paths, nil
}
