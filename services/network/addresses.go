package network

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
)

// Addresses is the network of a substrate whose frames end in the daemon's own stack: a lease per sandbox, no host side, and the policy applied to the stack's judge instead of the host.
type Addresses struct {
	pool    *pool
	subnet  netip.Prefix
	gateway netip.Addr
	egress  EgressSource
	judge   Judge
	// ensure serializes compile and apply, so an older snapshot never lands after a newer one; the host Service has the same lock.
	ensure sync.Mutex
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

	return &Addresses{pool: p, subnet: cfg.Subnet, gateway: gateway, egress: cfg.Egress}, nil
}

// Gateway is the one address the stack answers for.
func (a *Addresses) Gateway() netip.Addr { return a.gateway }

// Judge is what the stack asks about a flow off its address, and it answers with the chains the last apply compiled.
func (a *Addresses) Judge(f netstack.Flow) netstack.Verdict { return a.judge.Judge(f) }

// Allocate leases an address, or answers the one the sandbox holds; the resolver is the gateway, since nothing else is reachable.
func (a *Addresses) Allocate(ctx context.Context, id string) (models.NetworkSpec, error) {
	if err := validName(id); err != nil {
		return models.NetworkSpec{}, err
	}

	address, _, err := a.pool.allocate(id)
	if err != nil {
		return models.NetworkSpec{}, err
	}
	if err := a.apply(ctx); err != nil {
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

// Reapply compiles every chain again and hands them to the judge; the stack asks it per flow, so nothing else needs to change.
func (a *Addresses) Reapply(ctx context.Context, id string) error {
	if err := validName(id); err != nil {
		return err
	}

	return a.apply(ctx)
}

// ReapplyAll is Reapply for a change that names no sandbox.
func (a *Addresses) ReapplyAll(ctx context.Context) error { return a.apply(ctx) }

func (a *Addresses) apply(ctx context.Context) error {
	a.ensure.Lock()
	defer a.ensure.Unlock()
	if a.egress == nil {
		a.judge.Apply(nil)

		return nil
	}
	chains, err := a.egress.Chains(ctx)
	if err != nil {
		return fmt.Errorf("compile the egress chains: %w", err)
	}
	a.judge.Apply(chains)

	return nil
}
