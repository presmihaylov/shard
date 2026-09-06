package egress

import (
	"net/netip"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

func sandbox(t *testing.T) models.Sandbox {
	t.Helper()

	return models.Sandbox{
		ID:        "sb",
		Address:   netip.MustParsePrefix("10.87.0.2/16"),
		CreatedAt: time.Unix(100, 0).UTC(),
	}
}

func TestHostDropReadsTheLineTheChainsWrote(t *testing.T) {
	drop, ok := hostDrop("shard-egress rule=2 IN=shard0 OUT=eth0 SRC=10.87.0.2 DST=203.0.113.7 PROTO=TCP DPT=25")
	if !ok {
		t.Fatal("hostDrop refused a line of ours")
	}

	if drop.source != "10.87.0.2" {
		t.Errorf("the source became %q", drop.source)
	}
	if drop.Source != SourceHost || drop.Verdict != string(models.ActionDeny) {
		t.Errorf("the record became %+v", drop.Record)
	}
	if drop.Rule != "2" || drop.Address != "203.0.113.7" || drop.Port != 25 {
		t.Errorf("the record became %+v", drop.Record)
	}
	if drop.Reason != "the host chains dropped a tcp packet" {
		t.Errorf("the reason became %q", drop.Reason)
	}
}

// A line with no rule is not one of ours, whatever else it carries.
func TestHostDropRefusesALineWithNoRule(t *testing.T) {
	if _, ok := hostDrop("shard-egress SRC=10.87.0.2 DST=203.0.113.7"); ok {
		t.Error("hostDrop took a line with no rule")
	}
}

func TestHostDropRefusesALineThatIsNotOurs(t *testing.T) {
	if _, ok := hostDrop("usb 1-1: new device"); ok {
		t.Error("hostDrop took a line the kernel wrote for something else")
	}
}

func TestHostDropNamesTheProtocolOfAPacketThatCarriesNone(t *testing.T) {
	drop, ok := hostDrop("shard-egress rule=2 SRC=10.87.0.2 DST=203.0.113.7")
	if !ok || drop.Reason != "the host chains dropped a raw packet" {
		t.Errorf("hostDrop gave %+v", drop.Record)
	}
}

func TestLogReaderOrdersBothSourcesByTime(t *testing.T) {
	log, _ := newLog(t)
	sb := sandbox(t)

	if err := log.Append(sb.ID, Record{Time: time.Unix(115, 0).UTC(), Source: SourceProxy, Verdict: "allow", Host: "api.example.com", Rule: "1"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := log.Append(sb.ID, Record{Time: time.Unix(110, 0).UTC(), Source: SourceHost, Verdict: "deny", Rule: "2"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	records, err := NewLogReader(log).Read(sb)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 2 || records[0].Source != SourceHost || records[1].Source != SourceProxy {
		t.Errorf("Read gave %+v", records)
	}
}
