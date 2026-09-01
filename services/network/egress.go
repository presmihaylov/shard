package network

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
)

// EgressSource says what every sandbox's policy compiles to; nil is no policy anywhere.
type EgressSource interface {
	Chains(ctx context.Context) ([]Chain, error)
	// Fronted is every live sandbox whose HTTP and HTTPS go through the proxy instead of straight out.
	Fronted(ctx context.Context) ([]netip.Addr, error)
}

// ProxyPorts is where the proxy listens on the gateway. A fronted sandbox's port 80 is sent to HTTP
// and its 443 to HTTPS, so the proxy knows which it was without asking the kernel.
type ProxyPorts struct {
	HTTP  int
	HTTPS int
}

// DefaultProxyPorts is what a host with no configuration uses.
var DefaultProxyPorts = ProxyPorts{HTTP: 18080, HTTPS: 18443}

// Chain is the egress of one sandbox, keyed by its address; the rules run in order and the rest is dropped.
type Chain struct {
	Address netip.Addr
	Rules   []Compiled
}

// Compiled is one rule with every name already resolved to prefixes. Protocol empty is every protocol.
type Compiled struct {
	Action   models.Action
	Protocol string
	Ports    []int
	Prefixes []netip.Prefix
}

// privateRanges is the floor under every policy: the host's networks and what a cloud puts one hop away.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8", "100.64.0.0/10"}

// Groups is what a group destination names. The private ranges are the floor, not a group.
var Groups = map[string][]netip.Prefix{
	"any": {netip.MustParsePrefix("0.0.0.0/0")},
}

// Private is the floor's ranges, for the proxy's own check of a resolved address.
var Private = mustPrefixes(privateRanges)

func mustPrefixes(cidrs []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}

	return prefixes
}

// ruleset is the whole host policy in two tables replaced in one transaction; docs/egress.md says why two.
func (s *Service) ruleset(chains []Chain, leases, fronted []netip.Addr) string {
	var b strings.Builder

	fmt.Fprintf(&b, "table %[1]s %[2]s\ndelete table %[1]s %[2]s\n", tableFamily, tableName)
	fmt.Fprintf(&b, "table %[1]s %[2]s\ndelete table %[1]s %[2]s\n\n", bridgeFamily, tableName)

	fmt.Fprintf(&b, "table %s %s {\n", tableFamily, tableName)

	// A fronted sandbox's web traffic is turned to the proxy before routing, so the forward hook never sees it.
	fmt.Fprintf(&b, "\tchain prerouting {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, address := range fronted {
		fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s tcp dport 80 dnat to %s:%d\n", s.cfg.Bridge, address, s.gateway, s.cfg.Proxy.HTTP)
		fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s tcp dport 443 dnat to %s:%d\n", s.cfg.Bridge, address, s.gateway, s.cfg.Proxy.HTTPS)
	}
	b.WriteString("\t}\n\n")

	fmt.Fprintf(&b, "\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s oifname != %q masquerade\n\t}\n\n", s.cfg.Subnet, s.cfg.Bridge)

	// Only the proxy's ports, and only from a sandbox the proxy fronts: the host stays out of reach. No proxy, refused.
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname %q ct state established,related accept\n", s.cfg.Bridge)
	for _, address := range fronted {
		fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s ip daddr %s tcp dport { %d, %d } accept\n", s.cfg.Bridge, address, s.gateway, s.cfg.Proxy.HTTP, s.cfg.Proxy.HTTPS)
	}
	fmt.Fprintf(&b, "\t\tiifname %q drop\n\t}\n\n", s.cfg.Bridge)

	// The hook keeps policy accept, so a table the host also filters in is not overridden: every drop is explicit.
	fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n")
	// Nothing dials into a sandbox: a new flow toward the bridge is dropped before the policy is asked.
	fmt.Fprintf(&b, "\t\tiifname %[1]q ct state established,related accept\n\t\toifname %[1]q ct state new drop\n\t\tiifname %[1]q jump egress\n\t}\n\n", s.cfg.Bridge)

	// The private floor comes before the chains, so no rule of a policy opens it.
	fmt.Fprintf(&b, "\tchain egress {\n")
	fmt.Fprintf(&b, "\t\toifname %q drop\n", s.cfg.Bridge)
	fmt.Fprintf(&b, "\t\tip daddr { %s } drop\n", strings.Join(privateRanges, ", "))
	// A routed packet arrives from the bridge, never from the port, so the address is what picks the chain.
	for _, chain := range chains {
		fmt.Fprintf(&b, "\t\tip saddr %s jump %s\n", chain.Address, chainName(s.hostInterface(chain.Address)))
	}
	b.WriteString("\t}\n")

	for _, chain := range chains {
		fmt.Fprintf(&b, "\n\tchain %s {\n", chainName(s.hostInterface(chain.Address)))
		for _, rule := range chain.Rules {
			fmt.Fprintf(&b, "\t\t%s\n", render(rule))
		}
		fmt.Fprintf(&b, "\t\tdrop\n\t}\n")
	}

	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "table %s %s {\n", bridgeFamily, tableName)

	// One sandbox never reaches another, whatever its policy says, and ARP is what would find it.
	fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n\t\tmeta ibrname %q drop\n\t}\n\n", s.cfg.Bridge)

	// Every leased port is pinned to its address, in IP and in ARP, so no sandbox can send as another.
	fmt.Fprintf(&b, "\tchain prerouting {\n\t\ttype filter hook prerouting priority filter; policy accept;\n")
	for _, address := range leases {
		fmt.Fprintf(&b, "\t\tiifname %[1]q ether type ip ip saddr != %[2]s drop\n", s.hostInterface(address), address)
		fmt.Fprintf(&b, "\t\tiifname %[1]q arp saddr ip != %[2]s drop\n", s.hostInterface(address), address)
	}
	b.WriteString("\t}\n}\n")

	return b.String()
}

func chainName(host string) string { return "egress_" + host }

// render is one nft rule. A rule that resolved to no prefix matches nothing, and nft refuses an empty
// set, so it is left out and the chain's own drop says what happens to what it named.
func render(rule Compiled) string {
	if len(rule.Prefixes) == 0 {
		return "# no address"
	}

	var parts []string

	cidrs := make([]string, 0, len(rule.Prefixes))
	for _, prefix := range rule.Prefixes {
		cidrs = append(cidrs, prefix.String())
	}
	parts = append(parts, fmt.Sprintf("ip daddr { %s }", strings.Join(cidrs, ", ")))

	if rule.Protocol != "" {
		parts = append(parts, "meta l4proto "+rule.Protocol)
	}
	if len(rule.Ports) != 0 {
		ports := make([]string, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
		parts = append(parts, fmt.Sprintf("%s dport { %s }", rule.Protocol, strings.Join(ports, ", ")))
	}

	verdict := "drop"
	if rule.Action == models.ActionAllow {
		verdict = "accept"
	}

	return strings.Join(append(parts, verdict), " ")
}
