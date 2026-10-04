package egress

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	// LogFile is the decision log of one sandbox, and LogRotated is the one file kept behind it.
	LogFile    = "egress.jsonl"
	LogRotated = "egress.jsonl.1"
	// logPerm keeps the log with the record beside it: a decision names a host, never a secret value.
	logPerm = 0o640
)

// Source says which part of the enforcement wrote a record: the proxy judges a request, the host drops a packet, the resolver judges a question.
const (
	SourceProxy = "proxy"
	SourceHost  = "host"
	SourceDNS   = "dns"
)

// Record is one egress decision, as a line of the log. It never carries a header, a body or a secret value.
type Record struct {
	Time    time.Time `json:"time"`
	Source  string    `json:"source" enum:"proxy,host,dns"`
	Verdict string    `json:"verdict" enum:"allow,deny"`
	Host    string    `json:"host,omitempty"`
	Port    int       `json:"port,omitempty"`
	Address string    `json:"address,omitempty"`
	// Rule is the effective rule's id, or one of private, default, none, missing and resolve.
	Rule     string `json:"rule"`
	RuleText string `json:"rule_text,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Dirs is the part of the sandbox repository the log needs: where one sandbox's own files live.
type Dirs interface {
	Dir(id string) (string, error)
}

// Log appends one line per decision to a sandbox's own file, and renames it before a line would take it past max.
type Log struct {
	dirs Dirs
	max  int64
	// mu orders every size check, rename and write against the opens of a read, so no file passes max and a read sees each file once.
	mu sync.Mutex
	// watched counts the renames of each log a follow reads, so the follow can tell a file went by unread.
	watched map[string]*watched
}

// watched is one log that follows read: how many read it, and how many times Append renamed it since the first began.
type watched struct {
	follows int
	renames uint64
}

const (
	// MaxLog is what one log may hold before Append renames it, and one renamed file is kept behind it.
	MaxLog = 8 << 20
	// TailRecords is the most records one read returns, so a read costs the daemon the same whatever the log holds.
	TailRecords = 10000
)

func NewLog(dirs Dirs) *Log { return &Log{dirs: dirs, max: MaxLog, watched: map[string]*watched{}} }

// Append writes one record. A decision that cannot be written closes the door: the caller refuses the
// request rather than let it out unlogged.
func (l *Log) Append(id string, record Record) error {
	dir, err := l.dirs.Dir(id)
	if err != nil {
		return err
	}

	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode the egress record of sandbox %s: %w", id, err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.rotate(dir, int64(len(line))); err != nil {
		return err
	}

	path := filepath.Join(dir, LogFile)

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, logPerm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// rotate renames the log when the next line would take it past max; a line longer than max still lands, alone in its file.
func (l *Log) rotate(dir string, next int64) error {
	path := filepath.Join(dir, LogFile)

	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() == 0 || info.Size()+next <= l.max {
		return nil
	}

	if err := os.Rename(path, filepath.Join(dir, LogRotated)); err != nil {
		return fmt.Errorf("rotate %s: %w", path, err)
	}

	if w := l.watched[dir]; w != nil {
		w.renames++
	}

	return nil
}

// Tail returns the newest records of the sandbox's log, at most TailRecords, rotated file first, and how many older ones it left out.
func (l *Log) Tail(id string) (_ []Record, cut int, err error) {
	dir, err := l.dirs.Dir(id)
	if err != nil {
		return nil, 0, err
	}

	rotated, current, err := l.open(dir)
	if err != nil {
		return nil, 0, err
	}
	defer func() { err = errors.Join(err, closeAll(rotated, current)) }()

	newest := newTail(TailRecords)
	for _, file := range []*os.File{rotated, current} {
		if file == nil {
			continue
		}

		// The last line may still be in the writer's hands, so only whole lines count.
		if _, err := newest.read(bufio.NewReader(file), file.Name()); err != nil {
			return nil, 0, err
		}
	}

	records, err := newest.records()
	if err != nil {
		return nil, 0, err
	}

	return records, newest.cut, nil
}

// open opens both files under the lock Append renames under, so a rotation between the two opens can neither skip a file nor give one twice.
func (l *Log) open(dir string) (rotated, current *os.File, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return openBoth(dir)
}

// openBoth is open, for a caller that holds the lock.
func openBoth(dir string) (rotated, current *os.File, err error) {
	rotated, err = openIfAny(filepath.Join(dir, LogRotated))
	if err != nil {
		return nil, nil, err
	}

	current, err = openIfAny(filepath.Join(dir, LogFile))
	if err != nil {
		return nil, nil, errors.Join(err, closeAll(rotated))
	}

	return rotated, current, nil
}

// openIfAny opens path for reading, and gives no file and no error when there is none yet.
func openIfAny(path string) (*os.File, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // a log with no line yet has no file, which is not a failure
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return file, nil
}

func closeAll(files ...*os.File) error {
	var errs []error
	for _, file := range files {
		if file == nil {
			continue
		}
		if err := file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", file.Name(), err))
		}
	}

	return errors.Join(errs...)
}

// tail keeps the newest whole lines of a read, raw, so a long log is never decoded or held whole.
type tail struct {
	lines [][]byte
	next  int
	cut   int
}

func newTail(max int) *tail { return &tail{lines: make([][]byte, 0, max)} }

func (t *tail) add(line []byte) {
	if len(t.lines) < cap(t.lines) {
		t.lines = append(t.lines, line)

		return
	}

	t.lines[t.next] = line
	t.next = (t.next + 1) % len(t.lines)
	t.cut++
}

// read adds every whole line of r, and returns what follows the last newline, which a writer may still be finishing.
func (t *tail) read(r *bufio.Reader, name string) ([]byte, error) {
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return line, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) == 0 {
			continue
		}
		t.add(line)
	}
}

// records decodes the kept lines, oldest first.
func (t *tail) records() ([]Record, error) {
	lines := slices.Concat(t.lines[t.next:], t.lines[:t.next])

	records := make([]Record, 0, len(lines))
	for _, line := range lines {
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decode a line of the egress log: %w", err)
		}
		records = append(records, record)
	}

	return records, nil
}

// Merge orders the proxy's records and the host's drops by time, oldest first, and keeps the order they
// arrived in when two share a time.
func Merge(records ...[]Record) []Record {
	var all []Record
	for _, set := range records {
		all = append(all, set...)
	}

	slices.SortStableFunc(all, func(a, b Record) int { return a.Time.Compare(b.Time) })

	return all
}
