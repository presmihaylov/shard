package vzvm

// HoldRecovery runs hold after a reconnect chose to thaw a freeze whose answer was lost, and before that thaw goes out.
func (p *Provider) HoldRecovery(hold func()) {
	p.recovering = hold
}

// FailLinkClose gives the running sandbox id a link whose close returns err.
func (p *Provider) FailLinkClose(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.machines[id].link = failingLink{err: err}
}

type failingLink struct{ err error }

func (l failingLink) Close() error { return l.err }
