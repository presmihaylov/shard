package vzvm

// HoldRecovery runs hold after a reconnect chose to thaw a freeze whose answer was lost, and before that thaw goes out.
func (p *Provider) HoldRecovery(hold func()) {
	p.recovering = hold
}
