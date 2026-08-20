// Package network gives every sandbox its own network namespace, an address from a pool and a way
// out through the host. Host netfilter is the policy of record: gVisor's netstack iptables do not
// survive a checkpoint and restore, so nothing a sandbox can reach may depend on rules inside it.
package network

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
)

// The defaults are what a host with no configuration gets.
const (
	DefaultBridge = "shard0"
	DefaultSubnet = "10.87.0.0/16"
)

// DefaultNameservers is what a sandbox resolves through. The host's own resolver is usually a
// loopback address that means nothing inside a namespace, so shard names a reachable one instead.
var DefaultNameservers = []netip.Addr{
	netip.MustParseAddr("1.1.1.1"),
	netip.MustParseAddr("8.8.8.8"),
}

// guestInterface is what the sandbox's own end of the veth pair is called inside its namespace.
const guestInterface = "eth0"

// hostInterfacePrefix names the host end. A name may hold 15 characters, so the address offset that
// follows it keeps every name unique and short enough for any IPv4 subnet.
const hostInterfacePrefix = "shardv"

// The nft table shard owns. Chunk 4 adds the egress policy to it; nothing else may write to it.
const (
	tableFamily = "inet"
	tableName   = "shard"
)

const leasesDir = "leases"

// Config is the host's network layout. The zero value is not usable; New fills in every default.
type Config struct {
	// Root is the shard state root, and the leases live under it.
	Root string
	// Bridge is the one host interface every sandbox is a port of.
	Bridge string
	// Subnet is the pool. Its first address is the gateway, which is the bridge.
	Subnet netip.Prefix
	// Nameservers is written into every sandbox's resolv.conf.
	Nameservers []netip.Addr
}

// Service allocates and releases a sandbox's network. It holds nothing in memory between calls, so
// a later shard process can release what this one allocated.
type Service struct {
	cfg     Config
	manager *netns.Manager
	pool    *pool
	gateway netip.Addr
}

// New validates the layout and finds the host's network binaries.
func New(cfg Config, manager *netns.Manager) (*Service, error) {
	if manager == nil {
		return nil, errors.New("the network service needs a netns manager")
	}

	cfg, gateway, err := normalise(cfg)
	if err != nil {
		return nil, err
	}

	p, err := newPool(filepath.Join(cfg.Root, "network", leasesDir), cfg.Subnet, gateway.Next())
	if err != nil {
		return nil, err
	}

	return &Service{cfg: cfg, manager: manager, pool: p, gateway: gateway}, nil
}

// normalise applies the defaults and refuses a subnet no sandbox could be addressed from.
func normalise(cfg Config) (Config, netip.Addr, error) {
	if !filepath.IsAbs(cfg.Root) {
		return Config{}, netip.Addr{}, fmt.Errorf("the network root must be an absolute path, got %q", cfg.Root)
	}

	if cfg.Bridge == "" {
		cfg.Bridge = DefaultBridge
	}
	if !cfg.Subnet.IsValid() {
		cfg.Subnet = netip.MustParsePrefix(DefaultSubnet)
	}
	if len(cfg.Nameservers) == 0 {
		cfg.Nameservers = DefaultNameservers
	}

	// IPv6 is not refused because it is hard; it is refused because nothing here was written for it.
	if !cfg.Subnet.Addr().Is4() {
		return Config{}, netip.Addr{}, fmt.Errorf("the sandbox subnet %s is not IPv4", cfg.Subnet)
	}

	if cfg.Subnet.Masked() != cfg.Subnet {
		return Config{}, netip.Addr{}, fmt.Errorf("the sandbox subnet %s has host bits set; use %s", cfg.Subnet, cfg.Subnet.Masked())
	}

	// A /30 holds the network address, the gateway, one sandbox and the broadcast address.
	if cfg.Subnet.Bits() > 30 {
		return Config{}, netip.Addr{}, fmt.Errorf("the sandbox subnet %s is too small to hold a gateway and a sandbox", cfg.Subnet)
	}

	return cfg, cfg.Subnet.Addr().Next(), nil
}

// Gateway is the bridge's own address, which every sandbox routes through.
func (s *Service) Gateway() netip.Addr { return s.gateway }

// Bridge is the host interface every sandbox hangs off.
func (s *Service) Bridge() string { return s.cfg.Bridge }

// Ensure builds the host side: the bridge, forwarding and the nft table. It is idempotent and cheap,
// so Allocate calls it rather than making a caller remember to.
func (s *Service) Ensure(ctx context.Context) error {
	if err := s.manager.EnsureBridge(ctx, s.cfg.Bridge, netip.PrefixFrom(s.gateway, s.cfg.Subnet.Bits())); err != nil {
		return err
	}

	if err := s.manager.EnableForwarding(); err != nil {
		return err
	}

	return s.manager.ApplyRuleset(ctx, ruleset(s.cfg.Bridge, s.cfg.Subnet))
}

// Allocate gives the sandbox a namespace, an address and a route out. It reports what the provider
// must join. A half-built network is torn down rather than left for an operator to find.
func (s *Service) Allocate(ctx context.Context, id string) (models.NetworkSpec, error) {
	if err := validName(id); err != nil {
		return models.NetworkSpec{}, err
	}

	if err := s.Ensure(ctx); err != nil {
		return models.NetworkSpec{}, err
	}

	address, held, err := s.pool.allocate(id)
	if err != nil {
		return models.NetworkSpec{}, err
	}

	built, err := netns.NamespaceExists(id)
	if err != nil {
		return models.NetworkSpec{}, err
	}

	// A lease and a namespace together mean an earlier call already built this, which is the sandbox
	// that stopped and started again. A namespace without a lease belongs to nobody, so it goes.
	if held && built {
		return s.spec(id, address), nil
	}

	if built {
		if err := s.manager.DeleteNamespace(ctx, id); err != nil {
			return models.NetworkSpec{}, err
		}
	}

	if err := s.attach(ctx, id, address); err != nil {
		return models.NetworkSpec{}, errors.Join(err, s.Release(ctx, id))
	}

	return s.spec(id, address), nil
}

// spec is what the provider joins. It is derived, so any shard process can rebuild it from the record.
func (s *Service) spec(id string, address netip.Addr) models.NetworkSpec {
	return models.NetworkSpec{
		NetnsPath:     netns.NamespacePath(id),
		Address:       netip.PrefixFrom(address, s.cfg.Subnet.Bits()),
		Gateway:       s.gateway,
		HostInterface: s.hostInterface(address),
		Nameservers:   s.cfg.Nameservers,
	}
}

func (s *Service) attach(ctx context.Context, id string, address netip.Addr) error {
	host := s.hostInterface(address)

	// The lease says the address is this sandbox's, so an interface a crashed run left is ours to replace.
	if err := s.manager.DeleteLink(ctx, host); err != nil {
		return err
	}

	if err := s.manager.AddNamespace(ctx, id); err != nil {
		return err
	}

	if err := s.manager.AddVeth(ctx, host, guestInterface, id); err != nil {
		return err
	}

	if err := s.manager.AttachBridge(ctx, host, s.cfg.Bridge); err != nil {
		return err
	}

	// Before the port comes up, so no frame is switched to another sandbox in the window between.
	if err := s.manager.IsolatePort(ctx, host); err != nil {
		return err
	}

	if err := s.manager.SetUp(ctx, host); err != nil {
		return err
	}

	return s.configureGuest(ctx, id, address)
}

// configureGuest addresses the namespace before the sandbox joins it: gVisor reads the interfaces it
// finds there once, at create, and builds its netstack from them.
func (s *Service) configureGuest(ctx context.Context, id string, address netip.Addr) error {
	if err := s.manager.SetUpIn(ctx, id, "lo"); err != nil {
		return err
	}

	if err := s.manager.AddAddressIn(ctx, id, guestInterface, netip.PrefixFrom(address, s.cfg.Subnet.Bits())); err != nil {
		return err
	}

	if err := s.manager.SetUpIn(ctx, id, guestInterface); err != nil {
		return err
	}

	return s.manager.AddDefaultRouteIn(ctx, id, guestInterface, s.gateway)
}

// Release drops the namespace, the link and the lease. It is idempotent, so a stop may always call it.
func (s *Service) Release(ctx context.Context, id string) error {
	if err := validName(id); err != nil {
		return err
	}

	address, found, err := s.pool.find(id)
	if err != nil {
		return err
	}

	// Deleting the namespace takes the guest end with it, and a veth pair dies with either end. The
	// link is deleted first anyway, because an allocation can fail before the pair ever moved.
	if found {
		if err := s.manager.DeleteLink(ctx, s.hostInterface(address)); err != nil {
			return err
		}
	}

	if err := s.manager.DeleteNamespace(ctx, id); err != nil {
		return err
	}

	return s.pool.release(id)
}

// hostInterface names the host end after the address's offset into the subnet, which is unique and
// fits the 15 characters an interface name may hold, where a sandbox id does not.
func (s *Service) hostInterface(address netip.Addr) string {
	offset := toUint32(address) - toUint32(s.cfg.Subnet.Addr())

	return fmt.Sprintf("%s%d", hostInterfacePrefix, offset)
}

func toUint32(address netip.Addr) uint32 {
	v4 := address.As4()

	return binary.BigEndian.Uint32(v4[:])
}

// ruleset is the whole of shard's host netfilter policy today. The table is replaced in one
// transaction, so no packet is ever seen by half of it.
//
// It has two jobs. Masquerade lets a private address reach the internet. The input chain stops a
// sandbox reaching the host's own services, which the bridge would otherwise put one hop away.
// SHARD-70 adds the forward chain that turns egress default-deny, and SHARD-71 opens the input chain
// for its proxy.
func ruleset(bridge string, subnet netip.Prefix) string {
	return fmt.Sprintf(`table %[1]s %[2]s
delete table %[1]s %[2]s

table %[1]s %[2]s {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[3]s oifname != "%[4]s" masquerade
	}

	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[4]s" ct state established,related accept
		iifname "%[4]s" drop
	}
}
`, tableFamily, tableName, subnet, bridge)
}

// validName refuses anything that is not one plain path component, because the id becomes the name
// of a bind mount under /var/run/netns and an argument to ip.
func validName(id string) error {
	const maxChars = 64

	if id == "" {
		return errors.New("the sandbox id is empty")
	}

	if len(id) > maxChars {
		return fmt.Errorf("the sandbox id %q is longer than %d characters", id, maxChars)
	}

	for _, c := range id {
		alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alphanumeric && c != '-' && c != '_' {
			return fmt.Errorf("the sandbox id %q holds %q, which is not a letter, a digit, - or _", id, c)
		}
	}

	return nil
}
