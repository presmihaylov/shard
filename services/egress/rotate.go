package egress

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	rotatedFile = "egress.jsonl.1"
	// defaultMaxEventsBytes bounds the live file; with the one kept generation a sandbox holds twice it.
	defaultMaxEventsBytes = 8 << 20
)

// Rotate moves an oversized live file behind the rotated name and drops the generation before it.
// The proxy opens the file per write, so a rename loses nothing: a racing write lands in the
// renamed file and the next one starts a fresh live file.
func (e *Events) Rotate(id string) error {
	dir, err := e.dir(id)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, eventsFile)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() < e.maxBytes {
		return nil
	}

	if err := os.Rename(path, filepath.Join(dir, rotatedFile)); err != nil {
		return fmt.Errorf("rotate %s: %w", path, err)
	}

	return nil
}
