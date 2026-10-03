package vzvm

import (
	"context"
	"time"
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

// FailLinkClose gives the running sandbox id a link whose close returns err.
func (p *Provider) FailLinkClose(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.machines[id].link = failingLink{err: err}
}

type failingLink struct{ err error }

func (l failingLink) Close() error { return l.err }
