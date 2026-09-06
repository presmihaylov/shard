package egress

import (
	"os"
	"path/filepath"
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

func TestAppendWritesOneLinePerDecisionAndReadGivesThemBack(t *testing.T) {
	log, _ := newLog(t)

	first := Record{Time: time.Unix(1, 0).UTC(), Source: SourceProxy, Verdict: "allow", Host: "api.example.com", Port: 443, Address: "203.0.113.7", Rule: "1", RuleText: "allow api.example.com"}
	second := Record{Time: time.Unix(2, 0).UTC(), Source: SourceProxy, Verdict: "deny", Host: "evil.example.net", Port: 443, Rule: "default", Reason: "no rule allowed it"}

	for _, record := range []Record{first, second} {
		if err := log.Append("sb", record); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	records, err := log.Read("sb")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("Read gave %d records", len(records))
	}
	if records[0] != first || records[1] != second {
		t.Errorf("Read gave %+v", records)
	}
}

func TestReadIsEmptyForASandboxThatDecidedNothing(t *testing.T) {
	log, _ := newLog(t)

	records, err := log.Read("sb")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("Read gave %+v", records)
	}
}

func TestRotateRenamesOnceTheLogPassesTheSizeAndReadKeepsBothFiles(t *testing.T) {
	log, dirs := newLog(t)

	old := Record{Time: time.Unix(1, 0).UTC(), Source: SourceProxy, Verdict: "allow", Rule: "1"}
	if err := log.Append("sb", old); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := log.Rotate("sb", 1<<20); err != nil {
		t.Fatalf("Rotate under the size: %v", err)
	}
	dir, _ := dirs.Dir("sb")
	if _, err := os.Stat(filepath.Join(dir, LogRotated)); err == nil {
		t.Fatal("Rotate renamed a log under the size")
	}

	if err := log.Rotate("sb", 1); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, LogFile)); err == nil {
		t.Fatal("the log is still there after the rotation")
	}

	fresh := Record{Time: time.Unix(2, 0).UTC(), Source: SourceProxy, Verdict: "deny", Rule: "default"}
	if err := log.Append("sb", fresh); err != nil {
		t.Fatalf("Append after the rotation: %v", err)
	}

	records, err := log.Read("sb")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 2 || records[0] != old || records[1] != fresh {
		t.Errorf("Read gave %+v, want the rotated file first", records)
	}
}

func TestRotateLeavesASandboxThatWroteNothing(t *testing.T) {
	log, _ := newLog(t)

	if err := log.Rotate("sb", 1); err != nil {
		t.Errorf("Rotate: %v", err)
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
