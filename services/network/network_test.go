package network

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/netns"
)

func newService(t *testing.T, cfg Config) *Service {
	t.Helper()

	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}

	// The zero manager runs nothing: every test here asks about the layout, not about the host.
	s, err := New(cfg, &netns.Manager{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return s
}

func TestNewNeedsAManager(t *testing.T) {
	if _, err := New(Config{Root: t.TempDir()}, nil); err == nil {
		t.Fatal("New accepted a service with no netns manager")
	}
}

func TestNewRefusesARelativeRoot(t *testing.T) {
	if _, err := New(Config{Root: "var/lib/shard"}, &netns.Manager{}); err == nil {
		t.Fatal("New accepted a relative root")
	}
}

func TestNewRefusesASubnetThatIsNotIPv4(t *testing.T) {
	cfg := Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("fd00::/64")}

	if _, err := New(cfg, &netns.Manager{}); err == nil {
		t.Fatal("New accepted an IPv6 subnet, which nothing here was written for")
	}
}

// 10.87.0.5/16 names one host, not a subnet, and every address derived from it would be wrong.
func TestNewRefusesASubnetWithHostBitsSet(t *testing.T) {
	cfg := Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("10.87.0.5/16")}

	_, err := New(cfg, &netns.Manager{})
	if err == nil {
		t.Fatal("New accepted a subnet with host bits set")
	}
	if !strings.Contains(err.Error(), "10.87.0.0/16") {
		t.Errorf("got %v, want the masked subnet named in the error", err)
	}
}

func TestNewRefusesASubnetWithNoRoomForASandbox(t *testing.T) {
	cfg := Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("10.87.0.0/31")}

	if _, err := New(cfg, &netns.Manager{}); err == nil {
		t.Fatal("New accepted a subnet that holds no sandbox")
	}
}

func TestTheGatewayIsTheFirstAddressOfTheSubnet(t *testing.T) {
	s := newService(t, Config{})

	if got := s.Gateway(); got != netip.MustParseAddr("10.87.0.1") {
		t.Errorf("got gateway %s, want 10.87.0.1", got)
	}
	if got := s.Bridge(); got != DefaultBridge {
		t.Errorf("got bridge %q, want %q", got, DefaultBridge)
	}
}

// An interface name may hold 15 characters, and a sandbox id is longer than that on its own.
func TestTheHostInterfaceIsNamedAfterTheAddressOffset(t *testing.T) {
	s := newService(t, Config{})

	cases := map[string]string{
		"10.87.0.2":     "shardv2",
		"10.87.1.0":     "shardv256",
		"10.87.255.254": "shardv65534",
	}

	for address, want := range cases {
		got := s.hostInterface(netip.MustParseAddr(address))
		if got != want {
			t.Errorf("got %q for %s, want %q", got, address, want)
		}
		if len(got) > 15 {
			t.Errorf("the interface name %q is longer than the 15 characters a name may hold", got)
		}
	}
}

func TestAllocateRefusesAnIdThatIsNotOnePathComponent(t *testing.T) {
	s := newService(t, Config{})

	if _, err := s.Allocate(t.Context(), "../../etc/passwd"); err == nil {
		t.Fatal("Allocate accepted an id that would escape the namespace directory")
	}
}

// The masquerade is what lets a private address reach the internet, and the input drop is what stops
// a sandbox reaching the host's own services one hop away over the bridge.
func TestTheRulesetMasqueradesAndKeepsTheHostToItself(t *testing.T) {
	got := newService(t, Config{}).ruleset(nil, nil)

	for _, want := range []string{
		"delete table inet shard",
		`ip saddr 10.87.0.0/16 oifname != "shard0" masquerade`,
		`iifname "shard0" ct state established,related accept`,
		`iifname "shard0" drop`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset has no %q:\n%s", want, got)
		}
	}
}

// The delete must come after the create, or nft refuses a ruleset the host has never seen.
func TestTheRulesetCreatesTheTableBeforeItDeletesIt(t *testing.T) {
	got := newService(t, Config{}).ruleset(nil, nil)

	create := strings.Index(got, "table inet shard\n")
	remove := strings.Index(got, "delete table inet shard")

	if create < 0 || remove < 0 || create > remove {
		t.Errorf("the ruleset is not idempotent:\n%s", got)
	}
}

// A host in a VPC already routes 10.0.0.0/8, and the bridge's /16 would take a slice of it away.
func TestConflictFindsARouteThatHoldsTheSubnet(t *testing.T) {
	subnet := netip.MustParsePrefix("10.87.0.0/16")
	routes := []netip.Prefix{
		netip.MustParsePrefix("172.17.0.0/16"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}

	route, found := conflict(subnet, routes)
	if !found {
		t.Fatal("conflict accepted a subnet the host already routes")
	}
	if want := netip.MustParsePrefix("10.0.0.0/8"); route != want {
		t.Errorf("got %s, want %s", route, want)
	}
}

func TestConflictFindsARouteInsideTheSubnet(t *testing.T) {
	subnet := netip.MustParsePrefix("10.87.0.0/16")

	if _, found := conflict(subnet, []netip.Prefix{netip.MustParsePrefix("10.87.4.0/24")}); !found {
		t.Fatal("conflict accepted a subnet that holds a host route")
	}
}

func TestConflictAcceptsRoutesElsewhere(t *testing.T) {
	subnet := netip.MustParsePrefix("10.87.0.0/16")
	routes := []netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("10.88.0.0/16"),
		netip.MustParsePrefix("172.17.0.1/32"),
	}

	if route, found := conflict(subnet, routes); found {
		t.Fatalf("conflict refused %s over the route %s", subnet, route)
	}
}

// Ensure runs on every Allocate, and the first one puts the subnet in the route table itself.
func TestConflictIgnoresTheBridgesOwnRoute(t *testing.T) {
	subnet := netip.MustParsePrefix("10.87.0.0/16")

	if route, found := conflict(subnet, []netip.Prefix{subnet}); found {
		t.Fatalf("conflict refused %s over its own route %s", subnet, route)
	}
}
