package egress

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeDirs gives every sandbox a directory of its own under one temporary root.
type fakeDirs struct {
	root string
}

func (f fakeDirs) Dir(id string) (string, error) {
	dir := filepath.Join(f.root, id)

	return dir, os.MkdirAll(dir, 0o700)
}

func newLog(t *testing.T) (*Log, fakeDirs) {
	t.Helper()

	dirs := fakeDirs{root: t.TempDir()}

	return NewLog(dirs), dirs
}

func TestAppendWritesOneLinePerDecisionAndTailGivesThemBack(t *testing.T) {
	log, _ := newLog(t)

	first := Record{Time: time.Unix(1, 0).UTC(), Source: SourceProxy, Verdict: "allow", Host: "api.example.com", Port: 443, Address: "203.0.113.7", Rule: "1", RuleText: "allow api.example.com"}
	second := Record{Time: time.Unix(2, 0).UTC(), Source: SourceProxy, Verdict: "deny", Host: "evil.example.net", Port: 443, Rule: "default", Reason: "no rule allowed it"}

	for _, record := range []Record{first, second} {
		if err := log.Append("sb", record); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	records, cut, err := log.Tail("sb")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(records) != 2 || cut != 0 {
		t.Fatalf("Tail gave %d records and cut %d", len(records), cut)
	}
	if records[0] != first || records[1] != second {
		t.Errorf("Tail gave %+v", records)
	}
}

func TestTailIsEmptyForASandboxThatDecidedNothing(t *testing.T) {
	log, _ := newLog(t)

	records, cut, err := log.Tail("sb")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(records) != 0 || cut != 0 {
		t.Errorf("Tail gave %+v and cut %d", records, cut)
	}
}

func TestAppendRenamesBeforeALineWouldPassTheCapAndTailReadsBothFiles(t *testing.T) {
	log, dirs := newLog(t)
	dir, err := dirs.Dir("sb")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}

	line, err := json.Marshal(numbered(0))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Room for three lines and a half, so the fourth must go to a fresh file.
	log.max = int64(len(line)+1)*3 + int64(len(line))/2

	for i := range 5 {
		if err := log.Append("sb", numbered(i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		for _, name := range []string{LogFile, LogRotated} {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Size() > log.max {
				t.Fatalf("after Append %d, %s holds %d bytes, past the cap of %d", i, name, info.Size(), log.max)
			}
		}
	}

	records, cut, err := log.Tail("sb")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got := ports(records); !slices.Equal(got, []int{0, 1, 2, 3, 4}) || cut != 0 {
		t.Errorf("Tail gave %v and cut %d, want 0 to 4, the rotated file first", got, cut)
	}
}

func TestTailKeepsTheNewestRecordsAndCountsWhatItLeftOut(t *testing.T) {
	log, dirs := newLog(t)
	dir, err := dirs.Dir("sb")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}

	writeLines(t, filepath.Join(dir, LogRotated), 0, 4)
	writeLines(t, filepath.Join(dir, LogFile), 4, TailRecords+3)

	records, cut, err := log.Tail("sb")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	got := ports(records)
	if len(got) != TailRecords || got[0] != 3 || got[len(got)-1] != TailRecords+2 || cut != 3 {
		t.Errorf("Tail gave %d records from %d to %d and cut %d, want %d from 3 and cut 3", len(got), got[0], got[len(got)-1], cut, TailRecords)
	}
}

// Append writes a line in one call, but a read may still land between its bytes.
func TestTailSkipsALastLineTheWriterHasNotFinished(t *testing.T) {
	log, dirs := newLog(t)
	if err := log.Append("sb", numbered(1)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	dir, err := dirs.Dir("sb")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, LogFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := file.WriteString(`{"time":"2026-01-01T00:00:00Z","sour`); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	records, _, err := log.Tail("sb")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got := ports(records); !slices.Equal(got, []int{1}) {
		t.Errorf("Tail gave %v, want only the whole line", got)
	}
}

// Both files are opened under the lock Append renames under, so a read never skips a file nor gives one twice.
func TestTailDuringRotationsGivesAnUnbrokenRun(t *testing.T) {
	log, _ := newLog(t)
	log.max = 256

	done := make(chan error)
	go func() {
		for i := range 3000 {
			if err := log.Append("sb", numbered(i)); err != nil {
				done <- err

				return
			}
		}
		done <- nil
	}()

	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Append: %v", err)
			}

			return
		default:
		}

		records, _, err := log.Tail("sb")
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		got := ports(records)
		for i := 1; i < len(got); i++ {
			if got[i] != got[i-1]+1 {
				t.Fatalf("Tail gave %d after %d, want an unbroken run", got[i], got[i-1])
			}
		}
	}
}

func TestMergeOrdersBothHalvesByTimeAndKeepsTheOrderOfATie(t *testing.T) {
	proxy := []Record{{Time: time.Unix(3, 0).UTC(), Source: SourceProxy, Rule: "a"}, {Time: time.Unix(1, 0).UTC(), Source: SourceProxy, Rule: "b"}}
	host := []Record{{Time: time.Unix(2, 0).UTC(), Source: SourceHost, Rule: "c"}, {Time: time.Unix(3, 0).UTC(), Source: SourceHost, Rule: "d"}}

	merged := Merge(proxy, host)

	var order []string
	for _, record := range merged {
		order = append(order, record.Rule)
	}
	if len(order) != 4 || order[0] != "b" || order[1] != "c" || order[2] != "a" || order[3] != "d" {
		t.Errorf("Merge gave %v", order)
	}
}

// numbered is a record whose port is its place in the log, so a test can see a gap or a repeat.
func numbered(i int) Record {
	return Record{Time: time.Unix(int64(i), 0).UTC(), Source: SourceProxy, Verdict: "deny", Port: i, Rule: "default"}
}

func ports(records []Record) []int {
	got := make([]int, 0, len(records))
	for _, record := range records {
		got = append(got, record.Port)
	}

	return got
}

func writeLines(t *testing.T, path string, from, to int) {
	t.Helper()

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for i := from; i < to; i++ {
		if err := encoder.Encode(numbered(i)); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), logPerm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
