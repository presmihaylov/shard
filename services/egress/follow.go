package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
)

// ErrSandboxGone ends a follow whose sandbox was removed under it, which is not a failure of the follow.
var ErrSandboxGone = errors.New("the sandbox was removed")

// followPoll is how often a follow looks for a new line. The file is append-only and rotated by
// rename, so a poll on the size and the inode is enough and there is no inotify to depend on.
const followPoll = 250 * time.Millisecond

// Follow yields the newest records the log holds, at most TailRecords, then every record appended after them, until the context ends or the sandbox is removed.
func (r *LogReader) Follow(ctx context.Context, sb models.Sandbox, yield func(Record) error) error {
	dir, err := r.log.dirs.Dir(sb.ID)
	if err != nil {
		return err
	}

	// The current file is opened before its lines are read, so a line appended in between is tailed rather than lost.
	rotated, current, err := r.log.open(dir)
	if err != nil {
		return err
	}

	tail := &tailFile{path: filepath.Join(dir, LogFile)}
	defer tail.close()

	start, err := tail.start(rotated, current)
	if err != nil {
		return err
	}

	for _, record := range Merge(start) {
		if err := yield(record); err != nil {
			return err
		}
	}

	return r.tail(ctx, dir, tail, yield)
}

func (r *LogReader) tail(ctx context.Context, dir string, tail *tailFile, yield func(Record) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(followPoll):
		}

		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return ErrSandboxGone
		}

		// A rotation renames the file under the open handle, so the rest of it is read before the new one.
		rotated, err := tail.rotated()
		if err != nil {
			return err
		}

		records, err := tail.records()
		if err != nil {
			return err
		}

		if rotated {
			if err := tail.reopen(); err != nil {
				return err
			}

			after, err := tail.records()
			if err != nil {
				return err
			}
			records = append(records, after...)
		}

		for _, record := range records {
			if err := yield(record); err != nil {
				return err
			}
		}
	}
}

// tailFile reads one append-only file from where the last read stopped. A half-written line is kept
// and finished on the next read, because a reader sees the file whatever the writer is in the middle of.
type tailFile struct {
	path    string
	file    *os.File
	reader  *bufio.Reader
	partial string
	ino     uint64
}

func (t *tailFile) open() error {
	file, err := os.Open(t.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", t.path, err)
	}

	return t.use(file)
}

// use makes file the one the tail reads, from where its offset stands.
func (t *tailFile) use(file *os.File) error {
	ino, err := inode(file)
	if err != nil {
		return errors.Join(err, closeAll(file))
	}

	t.file, t.reader, t.partial, t.ino = file, bufio.NewReader(file), "", ino

	return nil
}

// start reads the newest whole lines of both files, and leaves the tail at the end of the current one.
func (t *tailFile) start(rotated, current *os.File) ([]Record, error) {
	newest := newTail(TailRecords)

	if rotated != nil {
		_, readErr := newest.read(bufio.NewReader(rotated), rotated.Name())
		if err := errors.Join(readErr, closeAll(rotated)); err != nil {
			return nil, errors.Join(err, closeAll(current))
		}
	}

	if current != nil {
		if err := t.use(current); err != nil {
			return nil, err
		}

		partial, err := newest.read(t.reader, t.path)
		if err != nil {
			return nil, err
		}
		t.partial = string(partial)
	}

	return newest.records()
}

// reopen moves to the file that took the name, once the renamed one has been read to its end.
func (t *tailFile) reopen() error {
	t.close()

	err := t.open()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

func (t *tailFile) close() {
	if t.file == nil {
		return
	}

	// A read-only handle gives nothing back on close that the follow could act on.
	_ = t.file.Close()
	t.file, t.reader = nil, nil
}

// rotated reports whether the name now holds a different file than the open handle.
func (t *tailFile) rotated() (bool, error) {
	// A log that had no line yet has no file to open, so the first one is picked up here.
	if t.file == nil {
		err := t.open()
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	info, err := os.Stat(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", t.path, err)
	}

	ino, ok := inodeOf(info)
	if !ok {
		return false, fmt.Errorf("stat %s: the host gave no inode", t.path)
	}

	return ino != t.ino, nil
}

// records reads every whole line written since the last call.
func (t *tailFile) records() ([]Record, error) {
	if t.file == nil {
		return nil, nil
	}

	var records []Record
	for {
		line, err := t.reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			t.partial += line

			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", t.path, err)
		}

		whole := t.partial + line
		t.partial = ""

		whole = strings.TrimRight(whole, "\n")
		if whole == "" {
			continue
		}

		var record Record
		if err := json.Unmarshal([]byte(whole), &record); err != nil {
			return nil, fmt.Errorf("decode a line of %s: %w", t.path, err)
		}
		records = append(records, record)
	}
}
