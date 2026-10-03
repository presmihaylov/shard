package vzvm

import (
	"context"
	"time"

	"github.com/presmihaylov/shard/pkg/vz"
)

// HoldRecovery runs hold after a reconnect chose to thaw a freeze whose answer was lost, and before that thaw goes out.
func (p *Provider) HoldRecovery(hold func()) {
	p.recovering = hold
}

// Probe asks a held shim for its state as each lookup does, with a bound a test can make short.
func (p *Provider) Probe(ctx context.Context, id string, bound time.Duration) {
	p.mu.Lock()
	m := p.machines[id]
	if m == nil {
		m = p.unadopted[id]
	}
	p.mu.Unlock()
	p.probe(ctx, m, bound)
}

// EndShim is the cleanup of a boot the provider could not finish.
func EndShim(id string, client *vz.Client, pid int) error {
	return endShim(id, client, pid)
}
