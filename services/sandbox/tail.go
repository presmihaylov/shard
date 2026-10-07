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

// openTail opens a log from its start: the rotated file the daemon keeps, then the log.
func openTail(path string) (*tail, error) { return openTailFrom(path, 0) }

// openTailFrom opens a log at offset, where one run began; a log not written yet opens on its first byte.
func openTailFrom(path string, offset int64) (*tail, error) {
	t := &tail{path: path}

	var err error
	if t.older, err = openLog(logfile.Rotated(path)); err != nil {
		return nil, err
	}

	t.rotated, err = rotatedAt(path)
	if err != nil {
		return nil, errors.Join(err, t.close())
	}

	if t.file, err = openLog(path); err != nil {
		return nil, errors.Join(err, t.close())
	}

	if err := t.seek(offset); err != nil {
		return nil, errors.Join(err, t.close())
	}

	return t, nil
}

// seek starts the read at offset: in the log while it holds that much, else in the rotated file it moved to; a start neither holds reads from the log's top.
func (t *tail) seek(offset int64) error {
	if offset == 0 {
		return nil
	}

	inLog, err := seekWithin(t.file, offset)
	if err != nil {
		return err
	}
	if inLog {
		return t.dropOlder()
	}

	inOlder, err := seekWithin(t.older, offset)
	if err != nil || inOlder {
		return err
	}

	return t.dropOlder()
}

// seekWithin seeks f to offset when f holds that much.
func seekWithin(f *os.File, offset int64) (bool, error) {
	if f == nil {
		return false, nil
	}

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	if info.Size() < offset {
		return false, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, fmt.Errorf("seek %s: %w", f.Name(), err)
	}

	return true, nil
}

func (t *tail) dropOlder() error {
	if t.older == nil {
		return nil
	}

	err := t.older.Close()
	t.older = nil
	if err != nil {
		return fmt.Errorf("close %s: %w", logfile.Rotated(t.path), err)
	}

	return nil
}

// openLog is nil for a log its writer has not created yet.
func openLog(path string) (*os.File, error) {
	f, err := logfile.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return f, nil
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

	if t.file == nil {
		return nil
	}

	return copyOutput(w, t.file)
}

// follow restarts from the top of the log when the daemon rotated it since the last read, then reads.
func (t *tail) follow(w io.Writer) error {
	if t.file == nil {
		return t.await(w)
	}

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

	next, err := logfile.Open(t.path)
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

// await reads a log its writer has created since the open, from its start.
func (t *tail) await(w io.Writer) error {
	if err := t.read(w); err != nil {
		return err
	}

	file, err := openLog(t.path)
	if err != nil || file == nil {
		return err
	}
	t.file = file

	if t.rotated, err = rotatedAt(t.path); err != nil {
		return err
	}

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
