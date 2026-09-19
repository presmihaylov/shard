package network

import (
	"context"
	"net/netip"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
)

// Addresses is the network of a substrate whose frames end in the daemon's own stack: a lease per sandbox, no host side and no rule to apply.
type Addresses struct {
	pool    *pool
	subnet  netip.Prefix
	gateway netip.Addr
}

// NewAddresses takes the same layout as New; the bridge and the nameservers in it are unused, since the host holds neither.
func NewAddresses(cfg Config) (*Addresses, error) {
	cfg, gateway, err := normalise(cfg)
	if err != nil {
		return nil, err
	}

	p, err := newPool(filepath.Join(cfg.Root, "network", leasesDir), cfg.Subnet, gateway.Next())
	if err != nil {
		return nil, err
	}

	return &Addresses{pool: p, subnet: cfg.Subnet, gateway: gateway}, nil
}

// Gateway is the one address the stack answers for.
func (a *Addresses) Gateway() netip.Addr { return a.gateway }

// Allocate leases an address, or answers the one the sandbox holds; the resolver is the gateway, since nothing else is reachable.
func (a *Addresses) Allocate(_ context.Context, id string) (models.NetworkSpec, error) {
	if err := validName(id); err != nil {
		return models.NetworkSpec{}, err
	}

	address, _, err := a.pool.allocate(id)
	if err != nil {
		return models.NetworkSpec{}, err
	}

	return models.NetworkSpec{
		Address:     netip.PrefixFrom(address, a.subnet.Bits()),
		Gateway:     a.gateway,
		Nameservers: []netip.Addr{a.gateway},
	}, nil
}

// Release drops the lease. It is idempotent, and delete is what calls it, so a stopped sandbox keeps its address.
func (a *Addresses) Release(_ context.Context, id string) error {
	if err := validName(id); err != nil {
		return err
	}

	return a.pool.release(id)
}

// Reapply has nothing to put on: the stack drops every frame but one to its own listeners, and the director judges those.
func (a *Addresses) Reapply(context.Context, string) error { return nil }

// ReapplyAll is Reapply for a change that names no sandbox.
func (a *Addresses) ReapplyAll(context.Context) error { return nil }
