package kmsg

import (
	"testing"
	"time"
)

func TestParseTurnsTheBootOffsetIntoWallTime(t *testing.T) {
	boot := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	line, err := parse("4,1234,1500000,-;shard-egress rule=default IN=shardv2\n", boot)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := boot.Add(1500 * time.Millisecond); !line.Time.Equal(want) {
		t.Errorf("the time is %s, want %s", line.Time, want)
	}
	if line.Message != "shard-egress rule=default IN=shardv2" {
		t.Errorf("the message is %q", line.Message)
	}
}

func TestParseRefusesARecordWithNoMessage(t *testing.T) {
	if _, err := parse("4,1234,1500000,-", time.Time{}); err == nil {
		t.Fatal("parse accepted a record with no message")
	}
}
