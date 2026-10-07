// Package logfile bounds a log file that only ever grows by appends, and keeps one older file beside it.
package logfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/presmihaylov/shard/pkg/store"
)

// Rotated names the one older file a log keeps beside it.
func Rotated(path string) string { return path + ".1" }

// Truncate bounds a log another process appends to, which a rename would leave writing the renamed file: past max, the last max bytes move to Rotated and the log is emptied in place.
func Truncate(path string, max int64) (err error) {
	f, err := openRegular(path, os.O_RDWR)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() <= max {
		return nil
	}

	tail := make([]byte, max)
	n, err := f.ReadAt(tail, info.Size()-max)
	if err != nil && (!errors.Is(err, io.EOF) || n < len(tail)) {
		return fmt.Errorf("read the tail of %s: %w", path, err)
	}

	// The writer holds the log with O_APPEND, so its next write lands at the new end, not over a hole at its old offset.
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate %s: %w", path, err)
	}

	// The rotated file changes after the truncate, so a follower that sees it change finds the log already emptied.
	if err := store.WriteFile(Rotated(path), tail, 0o600); err != nil {
		return fmt.Errorf("keep the tail of %s: %w", path, err)
	}

	return nil
}
