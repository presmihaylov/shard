package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	roomFor(t, log, 2)
	if err := log.Append("sb", followed(t, 10, "before")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	records, _, _ := startFollow(t, log, followSandbox())
	waitFor(t, 1, records.rules)

	// The second line goes in just before the rename, which is the one a naive reopen drops.
	for i, rule := range []string{"racing", "after"} {
		if err := log.Append("sb", followed(t, int64(20+10*i), rule)); err != nil {
			t.Fatalf("Append %s: %v", rule, err)
		}
	}

	dir, err := dirs.Dir("sb")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, LogRotated)); err != nil {
		t.Fatalf("the log did not rotate: %v", err)
	}

	rules := waitFor(t, 3, records.rules)
	if !slices.Equal(rules, []string{"before", "racing", "after"}) {
		t.Errorf("the follow gave %v across the rotation", rules)
	}
}

// Two renames before the next poll leave the middle file at .1, and the follow must read it before the newest.
func TestFollowLosesNothingToTwoRotationsWhileItIsHeld(t *testing.T) {
	log, _ := newLog(t)
	roomFor(t, log, 2)
	appendRules(t, log, 0, 2)

	held, done := startHeldFollow(t, log)
	appendRules(t, log, 2, 6)
	if got := onDisk(t, log); !slices.Equal(got, []string{"r2", "r3", "r4", "r5"}) {
		t.Fatalf("the log holds %v, want two rotations", got)
	}
	close(held.open)

	rules := waitFor(t, 6, held.rules)
	if want := []string{"r0", "r1", "r2", "r3", "r4", "r5"}; !slices.Equal(rules, want) {
		t.Errorf("the follow gave %v, want %v", rules, want)
	}

	select {
	case err := <-done:
		t.Errorf("the follow ended with %v", err)
	default:
	}
}

// A third rename unlinks a file the follow never opened, so it ends rather than skip it in silence.
func TestFollowEndsWhenAFileIsRenamedAwayUnread(t *testing.T) {
	log, _ := newLog(t)
	roomFor(t, log, 2)
	appendRules(t, log, 0, 2)

	held, done := startHeldFollow(t, log)
	appendRules(t, log, 2, 7)
	if got := onDisk(t, log); !slices.Equal(got, []string{"r4", "r5", "r6"}) {
		t.Fatalf("the log holds %v, want three rotations", got)
	}
	close(held.open)

	select {
	case err := <-done:
		if !errors.Is(err, errFellBehind) || !strings.Contains(err.Error(), "1 of its files") {
			t.Errorf("the follow ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow went on past a file it never read")
	}

	if rules := held.rules(); !slices.Equal(rules, []string{"r0", "r1"}) {
		t.Errorf("the follow gave %v before it ended", rules)
	}
	if len(log.watched) != 0 {
		t.Errorf("the log still counts renames for %d ended follows", len(log.watched))
	}
}

// held keeps a follow inside its first yield until the test closes open.
type held struct {
	collector
	ctx       context.Context
	entered   chan struct{}
	open      chan struct{}
	enterOnce sync.Once
}

func (h *held) yield(record Record) error {
	h.enterOnce.Do(func() {
		close(h.entered)
		select {
		case <-h.open:
		case <-h.ctx.Done():
		}
	})

	return h.collector.yield(record)
}

func startHeldFollow(t *testing.T, log *Log) (*held, chan error) {
	t.Helper()

	h := &held{ctx: t.Context(), entered: make(chan struct{}), open: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- NewLogReader(log).Follow(t.Context(), followSandbox(), h.yield) }()

	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the follow never yielded")
	}

	return h, done
}

// roomFor caps each file of the log at n records of followed, so the next Append renames it.
func roomFor(t *testing.T, log *Log, n int64) {
	t.Helper()

	line, err := json.Marshal(followed(t, 0, "r0"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	log.max = int64(len(line)+1)*n + int64(len(line))/2
}

func appendRules(t *testing.T, log *Log, from, to int) {
	t.Helper()

	for i := from; i < to; i++ {
		if err := log.Append("sb", followed(t, int64(i), fmt.Sprintf("r%d", i))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

func onDisk(t *testing.T, log *Log) []string {
	t.Helper()

	records, _, err := log.Tail("sb")
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	rules := make([]string, 0, len(records))
	for _, record := range records {
		rules = append(rules, record.Rule)
	}

	return rules
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
