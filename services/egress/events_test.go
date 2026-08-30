package egress

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/kmsg"
)

func TestEventsAreAppendedAndReadBackInOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sb"), 0o700); err != nil {
		t.Fatal(err)
	}
	events := NewEvents(func(id string) (string, error) { return filepath.Join(root, id), nil })

	if got, err := events.Read("sb"); err != nil || len(got) != 0 {
		t.Fatalf("a sandbox with no events read %v, %v", got, err)
	}

	first := models.EgressEvent{Time: time.Unix(10, 0).UTC(), Sandbox: "sb", Source: models.EgressSourceProxy, Verdict: models.ActionAllow, Destination: "api.example.com:443", Rule: "0"}
	second := models.EgressEvent{Time: time.Unix(11, 0).UTC(), Sandbox: "sb", Source: models.EgressSourceProxy, Verdict: models.ActionDeny, Destination: "other.example.com:443", Rule: RuleDefault, Reason: "no rule"}
	for _, ev := range []models.EgressEvent{first, second} {
		if err := events.Record(ev); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, err := events.Read("sb")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("Read = %+v, want the two events in order", got)
	}
}

func TestHostEventsReadTheDropsOfOneSandboxAndNameTheRule(t *testing.T) {
	sb := models.Sandbox{ID: "sb", Address: netip.MustParsePrefix("10.87.0.2/16")}
	eff := Effective{Policy: "locked", Rules: []EffectiveRule{
		{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationCIDR, Value: "1.1.1.1"}}},
		{Rule: models.Rule{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}, Protocol: "tcp", Ports: []int{22}}},
	}}
	at := time.Unix(100, 0)
	lines := []kmsg.Line{
		{Time: at, Message: "shard-egress rule=1 IN=shard0 OUT=eth0 MAC=00:11 SRC=10.87.0.2 DST=8.8.8.8 LEN=60 PROTO=TCP SPT=40000 DPT=22 WINDOW=64240 SYN"},
		{Time: at, Message: "shard-egress rule=default IN=shard0 OUT=eth0 SRC=10.87.0.3 DST=8.8.8.8 PROTO=ICMP TYPE=8"},
		{Time: at, Message: "shard-egress rule=private IN=shard0 OUT=eth0 SRC=10.87.0.2 DST=10.0.0.5 PROTO=ICMP TYPE=8"},
		{Time: at, Message: "shard0: port 1(shardv2) entered blocking state"},
	}

	got := HostEvents(lines, sb, eff, "shard-egress")
	if len(got) != 2 {
		t.Fatalf("HostEvents = %+v, want the two drops of shardv2", got)
	}
	want := models.EgressEvent{Time: at, Sandbox: "sb", Source: models.EgressSourceHost, Verdict: models.ActionDeny, Protocol: "tcp", Destination: "8.8.8.8:22", Address: "8.8.8.8", Rule: "1", RuleText: "deny group:any tcp:22"}
	if got[0] != want {
		t.Errorf("the ssh drop reads %+v, want %+v", got[0], want)
	}
	if got[1].Rule != RulePrivate || got[1].Destination != "10.0.0.5" || got[1].Protocol != "icmp" || got[1].RuleText != "" {
		t.Errorf("the private drop reads %+v", got[1])
	}
}

func TestMergeOrdersBothSourcesByTime(t *testing.T) {
	proxied := []models.EgressEvent{{Time: time.Unix(3, 0), Source: models.EgressSourceProxy}}
	host := []models.EgressEvent{{Time: time.Unix(1, 0), Source: models.EgressSourceHost}, {Time: time.Unix(5, 0), Source: models.EgressSourceHost}}

	got := Merge(proxied, host)
	if len(got) != 3 || got[0].Time != time.Unix(1, 0) || got[1].Source != models.EgressSourceProxy || got[2].Time != time.Unix(5, 0) {
		t.Errorf("Merge = %+v", got)
	}
}

func TestTextSpellsARuleAsPolicyCreateTakesIt(t *testing.T) {
	rule := EffectiveRule{Rule: models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}}, Implied: "secret TOKEN"}
	if got := rule.Text(); got != "allow domain:api.example.com tcp:80,443 (secret TOKEN)" {
		t.Errorf("Text = %q", got)
	}
}
