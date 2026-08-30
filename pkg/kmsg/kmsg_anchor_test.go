package kmsg

import (
	"testing"
	"time"
)

func TestLinesDateTheRingAgainstTheMarkAndDropTheMarks(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	records := []record{
		{sinceBoot: 10 * time.Second, message: "shard-kmsg-mark 1-1"},
		{sinceBoot: 40 * time.Second, message: "shard-egress rule=1 IN=shard0"},
		{sinceBoot: 100 * time.Second, message: "shard-kmsg-mark 1-2"},
	}

	got, err := lines(records, "shard-kmsg-mark 1-2", now)
	if err != nil {
		t.Fatalf("lines: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1: %v", len(got), got)
	}
	if want := now.Add(-60 * time.Second); !got[0].Time.Equal(want) {
		t.Errorf("the time is %s, want %s", got[0].Time, want)
	}
}

func TestLinesRefuseARingWithoutTheMark(t *testing.T) {
	if _, err := lines([]record{{message: "other"}}, "shard-kmsg-mark 1-2", time.Time{}); err == nil {
		t.Fatal("lines accepted a ring without the mark")
	}
}
