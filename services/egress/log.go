package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// LogFile is the decision log of one sandbox, and LogRotated is the one file kept behind it.
	LogFile    = "egress.jsonl"
	LogRotated = "egress.jsonl.1"
	// logPerm keeps the log with the record beside it: a decision names a host, never a secret value.
	logPerm = 0o640
)

// Source says which half of the enforcement wrote a record: the proxy judges a request, the host drops a packet.
const (
	SourceProxy = "proxy"
	SourceHost  = "host"
)

// Record is one egress decision, as a line of the log. It never carries a header, a body or a secret value.
type Record struct {
	Time    time.Time `json:"time"`
	Source  string    `json:"source"`
	Verdict string    `json:"verdict"`
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

// Log appends one line per decision to a sandbox's own file. Every write is O_APPEND, so the rotation
// task can rename the file under an open writer without losing a line.
type Log struct {
	dirs Dirs
}

func NewLog(dirs Dirs) *Log { return &Log{dirs: dirs} }

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

	path := filepath.Join(dir, LogFile)

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, logPerm)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// Read returns every record the sandbox's log holds, the rotated file first, oldest first.
func (l *Log) Read(id string) ([]Record, error) {
	dir, err := l.dirs.Dir(id)
	if err != nil {
		return nil, err
	}

	var records []Record
	for _, name := range []string{LogRotated, LogFile} {
		read, err := readRecords(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		records = append(records, read...)
	}

	return records, nil
}

func readRecords(path string) ([]Record, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var records []Record
	for line := range strings.SplitSeq(strings.TrimRight(string(blob), "\n"), "\n") {
		if line == "" {
			continue
		}

		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, fmt.Errorf("decode a line of %s: %w", path, err)
		}
		records = append(records, record)
	}

	return records, nil
}

// Rotate renames the log once it passes max bytes and keeps one file behind it. An O_APPEND writer that
// holds the old file keeps writing into it, and Read prints that file first, so no line is lost.
func (l *Log) Rotate(id string, max int64) error {
	dir, err := l.dirs.Dir(id)
	if err != nil {
		return err
	}

	path := filepath.Join(dir, LogFile)

	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() < max {
		return nil
	}

	if err := os.Rename(path, filepath.Join(dir, LogRotated)); err != nil {
		return fmt.Errorf("rotate %s: %w", path, err)
	}

	return nil
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
