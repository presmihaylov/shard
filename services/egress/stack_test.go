package egress

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/services/network"
)

var stackGateway = netip.MustParseAddr("10.87.0.1")

// A stack drop takes the shape of a host drop, with the rule the host chains would have named.
func TestStackDropReadsLikeAHostDrop(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		destination string
		rule, want  string
	}{
		{"203.0.113.7", network.RuleStack, "the stack dropped a tcp packet: a VM reaches nothing off the daemon except through the proxy"},
		{"10.87.0.1", network.RuleLocal, "the stack dropped a tcp packet to the host's own address"},
		{"192.168.1.10", network.RulePrivate, "the stack dropped a tcp packet to a private address"},
	} {
		got := StackDrop(stackGateway, netstack.Drop{Time: now, Guest: netip.MustParseAddr("10.87.0.2"), Destination: netip.MustParseAddr(tc.destination), Protocol: "tcp", Port: 25})
		want := Record{Time: now, Source: SourceHost, Verdict: string(models.ActionDeny), Port: 25, Address: tc.destination, Rule: tc.rule, Reason: tc.want}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", tc.destination, got, want)
		}
	}
}

// A stack drop lands in the sandbox that holds the guest address, and one from a guest no record holds lands nowhere.
func TestTailerWritesAStackDropIntoItsSandbox(t *testing.T) {
	sb := sandbox(t)
	tailer, _, decisions := newTailer(t, io.Discard, sb)

	record := StackDrop(stackGateway, netstack.Drop{Time: time.Unix(110, 0).UTC(), Guest: sb.Address.Addr(), Destination: netip.MustParseAddr("203.0.113.7"), Protocol: "tcp", Port: 25})
	if err := tailer.Drop(sb.Address.Addr(), record); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := tailer.Drop(netip.MustParseAddr("10.87.0.9"), record); err != nil {
		t.Fatalf("Drop of a stranger: %v", err)
	}

	records, err := decisions.Read(sb.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 || records[0] != record {
		t.Fatalf("the log holds %+v", records)
	}
}
