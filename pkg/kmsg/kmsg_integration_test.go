//go:build integration

package kmsg

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// The ring is the one thing no unit test can stand in for: only a real read proves the mark and the
// dating. The EPIPE path is not provoked here, because it needs the kernel to overwrite the record the
// reader is on, which no test can ask for; it is counted, not returned, so a miss loses records and
// never the follower.
func TestFollowWalksTheRingAndDatesItAgainstTheMark(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("reading /dev/kmsg needs root")
	}

	follower, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer follower.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	mark := "shard-kmsg-itest " + strconv.FormatInt(time.Now().UnixNano(), 10)
	putInTheRing(t, mark)

	var records []Record
	err = follower.Follow(ctx, func(record Record) error {
		records = append(records, record)
		if record.Message == mark {
			cancel()
		}

		return nil
	}, func() {})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("Follow: %v", err)
	}

	last := records[len(records)-1]
	if last.Message != mark {
		t.Fatalf("the follow ended on %+v, not on the mark it was waiting for", last)
	}
	if since := time.Since(last.Time); since < 0 || since > time.Minute {
		t.Errorf("the last record is dated %s, which is %s ago", last.Time, since)
	}

	for i := 1; i < len(records); i++ {
		if records[i].Sequence <= records[i-1].Sequence {
			t.Fatalf("the ring came back out of order at %d: %+v", i, records[i])
		}
	}
}

func TestFollowStopsWhenTheContextEnds(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("reading /dev/kmsg needs root")
	}

	follower, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer follower.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := follower.Follow(ctx, func(Record) error { return nil }, func() {}); err == nil {
		t.Fatal("Follow ran on past a context that had ended")
	}
}

// putInTheRing writes one line the follow can wait for. It goes in twice, because a reader sees a
// record only once the next one is reserved.
func putInTheRing(t *testing.T, line string) {
	t.Helper()

	file, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/kmsg to write: %v", err)
	}
	defer file.Close()

	for range 2 {
		if _, err := file.WriteString("<7>" + line); err != nil {
			t.Fatalf("write to /dev/kmsg: %v", err)
		}
	}
}
