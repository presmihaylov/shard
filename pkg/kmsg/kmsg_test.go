package kmsg

import (
	"strings"
	"testing"
	"time"
)

func TestParseReadsARecordAndKeepsTheMessageOnly(t *testing.T) {
	record, err := parse("4,231,987654321,-;shard-egress rule=2 SRC=10.87.0.2 DST=203.0.113.7")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if record.Priority != 4 || record.Sequence != 231 {
		t.Errorf("the head became %+v", record)
	}
	if record.Uptime != 987654321*time.Microsecond {
		t.Errorf("the uptime became %s", record.Uptime)
	}
	if record.Message != "shard-egress rule=2 SRC=10.87.0.2 DST=203.0.113.7" {
		t.Errorf("the message became %q", record.Message)
	}
}

// A kernel record may carry a dictionary of continuation lines, and none of them is the message.
func TestParseDropsTheDictionaryOfARecord(t *testing.T) {
	record, err := parse("6,12,1000,-;usb 1-1: new device\n SUBSYSTEM=usb\n DEVICE=+usb:1-1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if record.Message != "usb 1-1: new device" {
		t.Errorf("the message became %q", record.Message)
	}
}

func TestParseRefusesARecordItCannotRead(t *testing.T) {
	for name, raw := range map[string]string{
		"no semicolon":    "6,12,1000,-",
		"a short head":    "6,12;hello",
		"a bad priority":  "x,12,1000,-;hello",
		"a bad sequence":  "6,x,1000,-;hello",
		"a bad timestamp": "6,12,x,-;hello",
	} {
		if _, err := parse(raw); err == nil {
			t.Errorf("parse(%s) returned no error", name)
		}
	}
}

func TestDateTurnsUptimeIntoWallTimeAgainstTheMark(t *testing.T) {
	mark := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	records := []Record{{Uptime: 90 * time.Second}, {Uptime: 130 * time.Second}}

	dated := date(records, 100*time.Second, mark)

	if !dated[0].Time.Equal(mark.Add(-10 * time.Second)) {
		t.Errorf("the record before the mark became %s", dated[0].Time)
	}
	if !dated[1].Time.Equal(mark.Add(30 * time.Second)) {
		t.Errorf("the record after the mark became %s", dated[1].Time)
	}
}

func TestClipShortensALongRecordForAnError(t *testing.T) {
	long := strings.Repeat("a", 100)
	if len(clip(long)) != 67 {
		t.Errorf("clip gave %d bytes", len(clip(long)))
	}
	if clip("short") != "short" {
		t.Errorf("clip changed a short record")
	}
}
