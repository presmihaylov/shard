//go:build integration

package kmsg

import (
	"os"
	"testing"
	"time"
)

// The ring is the one thing no unit test can stand in for: only a real read proves the mark and the dating.
func TestReadWalksTheRingAndDatesItAgainstTheMark(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("reading /dev/kmsg needs root")
	}

	records, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("the ring gave no record")
	}

	last := records[len(records)-1]
	if last.Time.IsZero() {
		t.Errorf("the last record carries no time: %+v", last)
	}
	if since := time.Since(last.Time); since < 0 || since > time.Hour {
		t.Errorf("the last record is dated %s, which is %s ago", last.Time, since)
	}

	for i := 1; i < len(records); i++ {
		if records[i].Sequence <= records[i-1].Sequence {
			t.Fatalf("the ring came back out of order at %d: %+v", i, records[i])
		}
	}
}
