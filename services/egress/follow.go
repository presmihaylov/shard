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

// errFellBehind ends a follow that can no longer give every record, because a file was renamed twice before it was opened.
var errFellBehind = errors.New("the follow fell behind the log")

// followPoll is how often a follow looks for a new line; the log counts every rename under its lock, so no inotify is needed.
const followPoll = 250 * time.Millisecond

// Follow yields the newest records the log holds, at most TailRecords, then every record appended after them, until the context ends, the sandbox is removed, or a file goes by unread.
func (r *LogReader) Follow(ctx context.Context, sb models.Sandbox, yield func(Record) error) error {
	dir, err := r.log.dirs.Dir(sb.ID)
	if err != nil {
		return err
	}

	// The current file is opened before its lines are read, so a line appended in between is tailed rather than lost.
	rotated, current, renames, err := r.log.follow(dir)
	if err != nil {
		return err
	}
	defer r.log.unfollow(dir)

	tail := &tailFile{path: filepath.Join(dir, LogFile)}
	defer tail.close()

	start, err := tail.start(rotated, current, renames)
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

		unread, err := r.log.since(dir, tail.next)
		if err != nil {
			return err
		}

		records, err := tail.advance(unread)
		if err != nil {
			return err
		}

		for _, record := range records {
			if err := yield(record); err != nil {
				return err
			}
		}

		if unread.lost > 0 {
			return fmt.Errorf("%w: %d of its files were renamed away unread; follow again", errFellBehind, unread.lost)
		}
	}
}

// unread is what a follow has not opened yet. Generation n of a log is the file that was current after its nth rename.
type unread struct {
	// generation is the current file's, and rotated holds the one before it.
	generation       uint64
	rotated, current *os.File
	// lost counts the generations a later rename unlinked before the follow opened them.
	lost uint64
}

// follow opens both files as open does, and counts the renames of dir from now until unfollow.
func (l *Log) follow(dir string) (rotated, current *os.File, renames uint64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	rotated, current, err = openBoth(dir)
	if err != nil {
		return nil, nil, 0, err
	}

	w := l.watched[dir]
	if w == nil {
		w = &watched{}
		l.watched[dir] = w
	}
	w.follows++

	return rotated, current, w.renames, nil
}

func (l *Log) unfollow(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	w := l.watched[dir]
	w.follows--
	if w.follows == 0 {
		delete(l.watched, dir)
	}
}

// since opens, under the lock Append renames under, every file of generation next or newer that is still on disk.
func (l *Log) since(dir string, next uint64) (unread, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.watched[dir].renames
	u := unread{generation: now}
	if now < next {
		return u, nil
	}

	// The rotated file is generation now-1; a rename unlinked every one before it.
	if now > next {
		u.lost = now - 1 - next
		if u.lost > 0 {
			return u, nil
		}

		rotated, err := openIfAny(filepath.Join(dir, LogRotated))
		if err != nil {
			return unread{}, err
		}
		u.rotated = rotated
	}

	current, err := openIfAny(filepath.Join(dir, LogFile))
	if err != nil {
		return unread{}, errors.Join(err, closeAll(u.rotated))
	}
	u.current = current

	return u, nil
}

// tailFile reads one append-only file from where the last read stopped. A half-written line is kept
// and finished on the next read, because a reader sees the file whatever the writer is in the middle of.
type tailFile struct {
	path    string
	file    *os.File
	reader  *bufio.Reader
	partial string
	// next is the oldest generation of the log the tail has not opened.
	next uint64
}

// use makes file the one the tail reads, from where its offset stands.
func (t *tailFile) use(file *os.File) {
	t.file, t.reader, t.partial = file, bufio.NewReader(file), ""
}

// start reads the newest whole lines of both files, and leaves the tail at the end of the current one.
func (t *tailFile) start(rotated, current *os.File, renames uint64) ([]Record, error) {
	newest := newTail(TailRecords)
	t.next = renames

	if rotated != nil {
		_, readErr := newest.read(bufio.NewReader(rotated), rotated.Name())
		if err := errors.Join(readErr, closeAll(rotated)); err != nil {
			return nil, errors.Join(err, closeAll(current))
		}
	}

	if current != nil {
		t.use(current)
		t.next++

		partial, err := newest.read(t.reader, t.path)
		if err != nil {
			return nil, err
		}
		t.partial = string(partial)
	}

	return newest.records()
}

// advance reads what the open file gained, then each newer generation in u, oldest first, and moves the tail to the newest.
func (t *tailFile) advance(u unread) ([]Record, error) {
	// A rename leaves the handle on the same file, and Append never writes to a renamed one, so its rest comes first.
	records, err := t.records()
	if err != nil {
		return nil, errors.Join(err, closeAll(u.rotated, u.current))
	}

	if u.generation < t.next || u.lost > 0 {
		return records, nil
	}

	if u.rotated != nil {
		older := &tailFile{path: u.rotated.Name()}
		older.use(u.rotated)

		renamed, readErr := older.records()
		if err := errors.Join(readErr, closeAll(u.rotated)); err != nil {
			return nil, errors.Join(err, closeAll(u.current))
		}
		records = append(records, renamed...)
	}

	t.close()
	t.next = u.generation
	if u.current == nil {
		return records, nil
	}

	t.use(u.current)
	t.next++

	newer, err := t.records()
	if err != nil {
		return nil, err
	}

	return append(records, newer...), nil
}

func (t *tailFile) close() {
	if t.file == nil {
		return
	}

	// A read-only handle gives nothing back on close that the follow could act on.
	_ = t.file.Close()
	t.file, t.reader = nil, nil
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
