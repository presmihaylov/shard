package egress

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// collector gathers what a follow yields, from the goroutine the follow runs on.
type collector struct {
	mu      sync.Mutex
	records []Record
}

func (c *collector) yield(record Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.records = append(c.records, record)

	return nil
}

func (c *collector) rules() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	rules := make([]string, 0, len(c.records))
	for _, record := range c.records {
		rules = append(rules, record.Rule)
	}

	return rules
}

func followed(t *testing.T, seconds int64, rule string) Record {
	t.Helper()

	return Record{Time: time.Unix(seconds, 0).UTC(), Source: SourceProxy, Verdict: "deny", Rule: rule}
}

// waitFor gives a poll of 250 ms room to run without pinning the test to one.
func waitFor(t *testing.T, want int, got func() []string) []string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rules := got(); len(rules) >= want {
			return rules
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("the follow gave %v, and %d records were expected", got(), want)

	return nil
}

func startFollow(t *testing.T, log *Log, sb models.Sandbox) (*collector, chan error, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	records, done := &collector{}, make(chan error, 1)
	go func() { done <- NewLogReader(log).Follow(ctx, sb, records.yield) }()

	return records, done, cancel
}

func followSandbox() models.Sandbox {
	return models.Sandbox{ID: "sb", Address: netip.MustParsePrefix("10.87.0.2/16"), CreatedAt: time.Unix(1, 0).UTC()}
}

func TestFollowGivesTheLinesTheLogAlreadyHeldFirst(t *testing.T) {
	log, _ := newLog(t)
	if err := log.Append("sb", followed(t, 10, "first")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	records, _, _ := startFollow(t, log, followSandbox())

	if rules := waitFor(t, 1, records.rules); rules[0] != "first" {
		t.Errorf("the follow started with %v", rules)
	}
}

func TestFollowGivesALineAppendedAfterItStarted(t *testing.T) {
	log, _ := newLog(t)
	records, _, _ := startFollow(t, log, followSandbox())

	if err := log.Append("sb", followed(t, 20, "later")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if rules := waitFor(t, 1, records.rules); rules[0] != "later" {
		t.Errorf("the follow gave %v", rules)
	}
}

// A rotation renames the file under the open handle, and the lines on both sides of it are the log.
func TestFollowLosesNothingToARotation(t *testing.T) {
	log, dirs := newLog(t)
	if err := log.Append("sb", followed(t, 10, "before")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	records, _, _ := startFollow(t, log, followSandbox())
	waitFor(t, 1, records.rules)

	dir, err := dirs.Dir("sb")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}

	// The line goes in between the append and the rename, which is the one a naive reopen drops.
	if err := log.Append("sb", followed(t, 20, "racing")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, LogFile), filepath.Join(dir, LogRotated)); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := log.Append("sb", followed(t, 30, "after")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rules := waitFor(t, 3, records.rules)
	if rules[0] != "before" || rules[1] != "racing" || rules[2] != "after" {
		t.Errorf("the follow gave %v across the rotation", rules)
	}
}

func TestFollowEndsWhenTheContextEnds(t *testing.T) {
	log, _ := newLog(t)
	_, done, cancel := startFollow(t, log, followSandbox())

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the follow ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow outlived its context")
	}
}

func TestFollowEndsWhenTheSandboxIsRemoved(t *testing.T) {
	log, dirs := newLog(t)
	if err := log.Append("sb", followed(t, 10, "first")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	records, done, _ := startFollow(t, log, followSandbox())
	waitFor(t, 1, records.rules)

	dir, err := dirs.Dir("sb")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrSandboxGone) {
			t.Errorf("the follow ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow outlived the sandbox")
	}
}
