package network

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/hostfw"
)

type fakeFirewall struct {
	zones, rules int
	hole         hostfw.Hole
	err          error
}

func (f *fakeFirewall) EnsureZone(_ context.Context, hole hostfw.Hole) error {
	f.zones++
	f.hole = hole

	return f.err
}

func (f *fakeFirewall) EnsureRules(_ context.Context, hole hostfw.Hole) error {
	f.rules++
	f.hole = hole

	return nil
}

// The rules come back on every call, since a ufw reload flushes them, and the permanent zone is made once.
func TestOpenFirewallMakesTheZoneOnceAndTheRulesEveryTime(t *testing.T) {
	firewall := &fakeFirewall{}
	s := newService(t, Config{Firewall: firewall})

	for range 3 {
		if err := s.OpenFirewall(t.Context()); err != nil {
			t.Fatalf("OpenFirewall: %v", err)
		}
	}
	if firewall.zones != 1 || firewall.rules != 3 {
		t.Errorf("zone made %d times and rules %d times, want 1 and 3", firewall.zones, firewall.rules)
	}
}

// A zone that failed is tried again on the next call, and the rules wait for it.
func TestOpenFirewallRetriesAZoneThatFailed(t *testing.T) {
	firewall := &fakeFirewall{err: errors.New("ZONE_CONFLICT")}
	s := newService(t, Config{Firewall: firewall})

	if err := s.OpenFirewall(t.Context()); err == nil || !strings.Contains(err.Error(), "ZONE_CONFLICT") {
		t.Fatalf("OpenFirewall = %v, want the zone's failure", err)
	}
	firewall.err = nil
	if err := s.OpenFirewall(t.Context()); err != nil {
		t.Fatalf("OpenFirewall after the zone recovered: %v", err)
	}
	if firewall.zones != 2 || firewall.rules != 1 {
		t.Errorf("zone tried %d times and rules %d times, want 2 and 1", firewall.zones, firewall.rules)
	}
}

// The hole is the bridge's resolver, its proxy and its routed traffic, under the names removal looks for.
func TestOpenFirewallLetsTheBridgeThrough(t *testing.T) {
	firewall := &fakeFirewall{}
	s := newService(t, Config{Firewall: firewall})

	if err := s.OpenFirewall(t.Context()); err != nil {
		t.Fatalf("OpenFirewall: %v", err)
	}

	hole := firewall.hole
	if hole.Name != FirewallName || hole.Interface != DefaultBridge || hole.Policy != FirewallPolicy {
		t.Errorf("the hole is %q on %q with policy %q", hole.Name, hole.Interface, hole.Policy)
	}
	var got []string
	for _, rule := range hole.Rules {
		got = append(got, rule.Chain+" "+strings.Join(rule.Match, " "))
	}
	want := []string{
		"INPUT -d 10.87.0.1/32 -i shard0 -p udp -m udp --dport 53",
		"INPUT -d 10.87.0.1/32 -i shard0 -p tcp -m tcp --dport 53",
		"INPUT -d 10.87.0.1/32 -i shard0 -p tcp -m multiport --dports 30080,30443",
		"FORWARD -i shard0",
		"FORWARD -o shard0 -m conntrack --ctstate RELATED,ESTABLISHED",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rules:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// No firewall configured is no call at all.
func TestOpenFirewallWithoutOneIsANoop(t *testing.T) {
	if err := newService(t, Config{}).OpenFirewall(t.Context()); err != nil {
		t.Errorf("OpenFirewall = %v, want nil", err)
	}
}
