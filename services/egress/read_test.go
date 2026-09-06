package egress

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/kmsg"
)

// fakeRing gives back canned lines, the way /dev/kmsg would on the box.
type fakeRing struct {
	records []kmsg.Record
	err     error
}

func (f fakeRing) Read() ([]kmsg.Record, error) { return f.records, f.err }

func sandbox(t *testing.T) models.Sandbox {
	t.Helper()

	return models.Sandbox{
		ID:        "sb",
		Address:   netip.MustParsePrefix("10.87.0.2/16"),
		CreatedAt: time.Unix(100, 0).UTC(),
	}
}

func line(at int64, message string) kmsg.Record {
	return kmsg.Record{Time: time.Unix(at, 0).UTC(), Message: message}
}

func TestHostDropsKeepsTheLinesThisSandboxWrote(t *testing.T) {
	sb := sandbox(t)
	records := HostDrops([]kmsg.Record{
		line(110, "shard-egress rule=2 IN=shard0 OUT=eth0 SRC=10.87.0.2 DST=203.0.113.7 PROTO=TCP DPT=25"),
		line(120, "shard-egress rule=default SRC=10.87.0.9 DST=203.0.113.7 PROTO=TCP DPT=25"),
		line(90, "shard-egress rule=private SRC=10.87.0.2 DST=192.168.0.1 PROTO=TCP DPT=22"),
		line(130, "usb 1-1: new device"),
	}, sb)

	if len(records) != 1 {
		t.Fatalf("HostDrops gave %+v", records)
	}

	record := records[0]
	if record.Source != SourceHost || record.Verdict != string(models.ActionDeny) {
		t.Errorf("the record became %+v", record)
	}
	if record.Rule != "2" || record.Address != "203.0.113.7" || record.Port != 25 {
		t.Errorf("the record became %+v", record)
	}
	if !record.Time.Equal(time.Unix(110, 0).UTC()) {
		t.Errorf("the time became %s", record.Time)
	}
	if record.Reason != "the host chains dropped a tcp packet" {
		t.Errorf("the reason became %q", record.Reason)
	}
}

// A line with no rule is not one of ours, whatever else it carries.
func TestHostDropsRefusesALineWithNoRule(t *testing.T) {
	if records := HostDrops([]kmsg.Record{line(110, "shard-egress SRC=10.87.0.2 DST=203.0.113.7")}, sandbox(t)); records != nil {
		t.Errorf("HostDrops gave %+v", records)
	}
}

func TestHostDropsGivesNothingForASandboxWithNoAddress(t *testing.T) {
	sb := sandbox(t)
	sb.Address = netip.Prefix{}

	if records := HostDrops([]kmsg.Record{line(110, "shard-egress rule=2 SRC=10.87.0.2 DST=203.0.113.7")}, sb); records != nil {
		t.Errorf("HostDrops gave %+v", records)
	}
}

func TestHostDropsNamesTheProtocolOfAPacketThatCarriesNone(t *testing.T) {
	records := HostDrops([]kmsg.Record{line(110, "shard-egress rule=2 SRC=10.87.0.2 DST=203.0.113.7")}, sandbox(t))
	if len(records) != 1 || records[0].Reason != "the host chains dropped a raw packet" {
		t.Errorf("HostDrops gave %+v", records)
	}
}

func TestLogReaderMergesTheProxyRecordsWithTheHostDrops(t *testing.T) {
	log, _ := newLog(t)
	sb := sandbox(t)

	if err := log.Append(sb.ID, Record{Time: time.Unix(115, 0).UTC(), Source: SourceProxy, Verdict: "allow", Host: "api.example.com", Rule: "1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ring := fakeRing{records: []kmsg.Record{line(110, "shard-egress rule=2 SRC=10.87.0.2 DST=203.0.113.7 PROTO=TCP DPT=25")}}

	records, err := NewLogReader(log, ring).Read(sb)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 2 || records[0].Source != SourceHost || records[1].Source != SourceProxy {
		t.Errorf("Read gave %+v", records)
	}
}

func TestLogReaderReportsARingItCannotRead(t *testing.T) {
	log, _ := newLog(t)

	_, err := NewLogReader(log, fakeRing{err: errors.New("no ring")}).Read(sandbox(t))
	if err == nil {
		t.Fatal("Read hid a ring it could not read")
	}
}
