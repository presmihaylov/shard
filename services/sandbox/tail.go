package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/presmihaylov/shard/pkg/logfile"
)

// tail reads a log the daemon rotates under it, by a rename when the daemon writes the log and by a truncate when a runtime does.
type tail struct {
	path  string
	older *os.File
	file  *os.File
	// rotated is the mtime of the rotated file at the last restart, which every rotation rewrites.
	rotated time.Time
	// awaiting is a truncate already restarted from before the rotated file it writes landed.
	awaiting bool
}

func openTail(path string) (*tail, error) {
	t := &tail{path: path}

	older, err := os.Open(logfile.Rotated(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open %s: %w", logfile.Rotated(path), err)
	}
	if err == nil {
		t.older = older
	}

	t.rotated, err = rotatedAt(path)
	if err != nil {
		return nil, errors.Join(err, t.close())
	}

	t.file, err = os.Open(path)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open %s: %w", path, err), t.close())
	}

	return t, nil
}

func (t *tail) close() error {
	var err error
	if t.older != nil {
		err = t.older.Close()
	}
	if t.file != nil {
		err = errors.Join(err, t.file.Close())
	}

	return err
}

// read writes the rotated file on the first call, then what the log gained since the last read.
func (t *tail) read(w io.Writer) error {
	if t.older != nil {
		err := errors.Join(copyOutput(w, t.older), t.older.Close())
		t.older = nil
		if err != nil {
			return err
		}
	}

	return copyOutput(w, t.file)
}

// follow restarts from the top of the log when the daemon rotated it since the last read, then reads.
func (t *tail) follow(w io.Writer) error {
	held, err := t.file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", t.path, err)
	}

	current, err := os.Stat(t.path)
	// The daemon renames the log before it creates the next one, so a log that is gone is a rotation half done.
	if errors.Is(err, fs.ErrNotExist) {
		return t.read(w)
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", t.path, err)
	}

	if !os.SameFile(held, current) {
		return t.reopen(w)
	}

	// A read before this check would take what a writer appended after a truncate from the old offset, not from 0.
	if err := t.rewindOnTruncate(held.Size()); err != nil {
		return err
	}

	return t.read(w)
}

// reopen drains the renamed file, which takes no write once the next one exists, and reads the next one from its start.
func (t *tail) reopen(w io.Writer) error {
	if err := t.read(w); err != nil {
		return err
	}

	next, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", t.path, err)
	}
	err = t.file.Close()
	t.file = next
	if err != nil {
		return fmt.Errorf("close the rotated %s: %w", t.path, err)
	}

	t.rotated, err = rotatedAt(t.path)
	if err != nil {
		return err
	}
	t.awaiting = false

	return copyOutput(w, t.file)
}

// rewindOnTruncate rewinds a log a runtime holds once per truncate, seen by the log shorter than the read or by the rotated file the truncate writes after it.
func (t *tail) rewindOnTruncate(size int64) error {
	offset, err := t.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("seek %s: %w", t.path, err)
	}

	rotated, err := rotatedAt(t.path)
	if err != nil {
		return err
	}
	changed := !rotated.Equal(t.rotated)
	t.rotated = rotated

	if size < offset {
		t.awaiting = !changed
		return t.rewind()
	}
	if !changed {
		return nil
	}
	if t.awaiting {
		t.awaiting = false
		return nil
	}

	return t.rewind()
}

func (t *tail) rewind() error {
	if _, err := t.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", t.path, err)
	}

	return nil
}

// rotatedAt is the mtime of the rotated file of a log, or zero before its first rotation.
func rotatedAt(path string) (time.Time, error) {
	info, err := os.Stat(logfile.Rotated(path))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("stat %s: %w", logfile.Rotated(path), err)
	}

	return info.ModTime(), nil
}
