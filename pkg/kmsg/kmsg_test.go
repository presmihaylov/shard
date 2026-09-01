package kmsg

import (
	"testing"
	"time"
)

func TestParseReadsTheBootOffset(t *testing.T) {
	r, err := parse("4,1234,1500000,-;shard-egress rule=default IN=shardv2\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.sinceBoot != 1500*time.Millisecond {
		t.Errorf("the offset is %s, want 1.5s", r.sinceBoot)
	}
	if r.message != "shard-egress rule=default IN=shardv2" {
		t.Errorf("the message is %q", r.message)
	}
}

func TestParseKeepsTheFirstLineOfARecordWithADictionary(t *testing.T) {
	r, err := parse("6,2,10,-;shard-egress rule=1 IN=shard0\n SUBSYSTEM=net\n DEVICE=+net:shard0\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.message != "shard-egress rule=1 IN=shard0" {
		t.Errorf("the message is %q", r.message)
	}
}

func TestParseRefusesARecordWithNoMessage(t *testing.T) {
	if _, err := parse("4,1234,1500000,-"); err == nil {
		t.Fatal("parse accepted a record with no message")
	}
}
