package network

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/presmihaylov/shard/pkg/dns"
	"github.com/presmihaylov/shard/pkg/hostfw"
	"github.com/presmihaylov/shard/pkg/proxy"
)

// HostFirewall lets the bridge through a firewall the host runs of its own, such as ufw or firewalld.
type HostFirewall interface {
	EnsureRules(ctx context.Context, hole hostfw.Hole) error
	EnsureZone(ctx context.Context, hole hostfw.Hole) error
}

// The zone and policy shard makes in the host's firewall, and the marker that tells its rules, zone and policy from the host's own.
const (
	FirewallName   = "shard"
	FirewallPolicy = "shard-forwarding"
	FirewallMarker = "managed-by-shard"
)

// OpenFirewall lets the resolver, the proxy and routed traffic past the host's firewall, as Docker does for its bridge.
func (s *Service) OpenFirewall(ctx context.Context) error {
	if s.cfg.Firewall == nil {
		return nil
	}

	s.firewall.Lock()
	defer s.firewall.Unlock()

	hole := s.hole()
	// The firewalld zone is permanent, so once is enough; a ufw reload flushes the rules, so every call checks them.
	if !s.zoned {
		if err := s.cfg.Firewall.EnsureZone(ctx, hole); err != nil {
			return fmt.Errorf("open the host firewall for %s: %w", s.cfg.Bridge, err)
		}
		s.zoned = true
	}
	if err := s.cfg.Firewall.EnsureRules(ctx, hole); err != nil {
		return fmt.Errorf("open the host firewall for %s: %w", s.cfg.Bridge, err)
	}

	return nil
}

// hole accepts only what shard's own chains then judge, so it opens no host port they would not.
func (s *Service) hole() hostfw.Hole {
	bridge := s.cfg.Bridge
	gateway := netip.PrefixFrom(s.gateway, s.gateway.BitLen()).String()
	port := strconv.Itoa(dns.Port)

	return hostfw.Hole{
		Name:      FirewallName,
		Marker:    FirewallMarker,
		Interface: bridge,
		Policy:    FirewallPolicy,
		Rules: []hostfw.Rule{
			{Chain: "INPUT", Match: []string{"-d", gateway, "-i", bridge, "-p", "udp", "-m", "udp", "--dport", port}},
			{Chain: "INPUT", Match: []string{"-d", gateway, "-i", bridge, "-p", "tcp", "-m", "tcp", "--dport", port}},
			{Chain: "INPUT", Match: []string{"-d", gateway, "-i", bridge, "-p", "tcp", "-m", "multiport", "--dports", fmt.Sprintf("%d,%d", proxy.PlainPort, proxy.TLSPort)}},
			{Chain: "FORWARD", Match: []string{"-i", bridge}},
			{Chain: "FORWARD", Match: []string{"-o", bridge, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED"}},
		},
	}
}
