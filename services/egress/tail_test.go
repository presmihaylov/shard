package egress

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/kmsg"
)

// fakeRing hands the tailer canned records the way /dev/kmsg would on the box, and then stops.
type fakeRing struct {
	records  []kmsg.Record
	caughtUp int
}

func (f *fakeRing) Follow(_ context.Context, yield func(kmsg.Record) error, caughtUp func()) error {
	for _, record := range f.records {
		if err := yield(record); err != nil {
			return err
		}
	}
	f.caughtUp++
	caughtUp()

	return nil
}

type fakeSandboxes struct {
	sandboxes []models.Sandbox
	listed    int
}

func (f *fakeSandboxes) List() ([]models.Sandbox, error) {
	f.listed++

	return f.sandboxes, nil
}

func newTailer(t *testing.T, out io.Writer, sandboxes ...models.Sandbox) (*Tailer, string, *Log) {
	t.Helper()

	root := t.TempDir()
	decisions := NewLog(fakeDirs{root: root})

	return NewTailer(root, decisions, &fakeSandboxes{sandboxes: sandboxes}, log.New(out, "", 0)), root, decisions
}

func drops(sequence uint64, at int64, rule string) kmsg.Record {
	return kmsg.Record{
		Sequence: sequence,
		Time:     time.Unix(at, 0).UTC(),
		Message:  "shard-egress rule=" + rule + " SRC=10.87.0.2 DST=203.0.113.7 PROTO=TCP DPT=25",
	}
}

func TestTailWritesEveryDropTheRingHolds(t *testing.T) {
	sb := sandbox(t)
	tailer, root, decisions := newTailer(t, io.Discard, sb)

	ring := &fakeRing{records: []kmsg.Record{drops(7, 110, "2"), drops(8, 120, "default")}}
	if err := tailer.Run(t.Context(), ring); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := decisions.Read(sb.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 2 || records[0].Rule != "2" || records[1].Rule != "default" {
		t.Fatalf("the log holds %+v", records)
	}
	if records[0].Source != SourceHost || !records[0].Time.Equal(time.Unix(110, 0).UTC()) {
		t.Errorf("the first record became %+v", records[0])
	}

	if cursor := readCursor(t, root); cursor != "8" {
		t.Errorf("the cursor holds %q", cursor)
	}
}

// The cursor is what a restart stands on: the ring still holds what was already written.
func TestTailSkipsTheSequencesTheCursorAlreadyNames(t *testing.T) {
	sb := sandbox(t)
	tailer, root, decisions := newTailer(t, io.Discard, sb)
	writeCursor(t, root, "8")

	ring := &fakeRing{records: []kmsg.Record{drops(7, 110, "2"), drops(8, 120, "3"), drops(9, 130, "4")}}
	if err := tailer.Run(t.Context(), ring); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := decisions.Read(sb.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 || records[0].Rule != "4" {
		t.Fatalf("the log holds %+v", records)
	}
}

// A duplicate line is better than a drop nobody ever sees, so an unreadable cursor writes the ring whole.
func TestTailWritesTheRingWholeWhenTheCursorCannotBeRead(t *testing.T) {
	sb := sandbox(t)
	var out strings.Builder
	tailer, root, decisions := newTailer(t, &out, sb)
	writeCursor(t, root, "not a sequence")

	if err := tailer.Run(t.Context(), &fakeRing{records: []kmsg.Record{drops(7, 110, "2")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := decisions.Read(sb.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the log holds %+v", records)
	}
	if !strings.Contains(out.String(), "holds no sequence") {
		t.Errorf("the tailer said %q", out.String())
	}
}

// A sandbox removed while the daemon was down leaves lines nothing can be done with. They are counted
// once, never logged one by one: the ring rate-limits at 2 lines a second per rule and no more.
func TestTailCountsTheDropsOfASandboxThatIsGone(t *testing.T) {
	var out strings.Builder
	tailer, _, decisions := newTailer(t, &out)

	if err := tailer.Run(t.Context(), &fakeRing{records: []kmsg.Record{drops(7, 110, "2"), drops(8, 120, "3")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := decisions.Read("sb")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("the log holds %+v", records)
	}
	if got := out.String(); !strings.Contains(got, "2 host drops") || strings.Count(got, "\n") != 1 {
		t.Errorf("the tailer said %q", got)
	}
}

// An address is reused, so a line older than the sandbox belongs to whoever held the address before it.
func TestTailLeavesADropOlderThanTheSandbox(t *testing.T) {
	sb := sandbox(t)
	tailer, _, decisions := newTailer(t, io.Discard, sb)

	if err := tailer.Run(t.Context(), &fakeRing{records: []kmsg.Record{drops(7, 90, "2")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := decisions.Read(sb.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("the log holds %+v", records)
	}
}

func TestTailLeavesTheLinesTheKernelWroteForSomethingElse(t *testing.T) {
	sb := sandbox(t)
	tailer, root, decisions := newTailer(t, io.Discard, sb)

	ring := &fakeRing{records: []kmsg.Record{{Sequence: 7, Time: time.Unix(110, 0).UTC(), Message: "usb 1-1: new device"}}}
	if err := tailer.Run(t.Context(), ring); err != nil {
		t.Fatalf("Run: %v", err)
	}

	records, err := decisions.Read(sb.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("the log holds %+v", records)
	}
	if _, err := os.Stat(filepath.Join(root, CursorFile)); err == nil {
		t.Error("a line that is not ours moved the cursor")
	}
}

func readCursor(t *testing.T, root string) string {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(root, CursorFile))
	if err != nil {
		t.Fatalf("read the cursor: %v", err)
	}

	return strings.TrimSpace(string(blob))
}

func writeCursor(t *testing.T, root, value string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(root, CursorFile), []byte(value), 0o640); err != nil {
		t.Fatalf("write the cursor: %v", err)
	}
}
