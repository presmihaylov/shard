package vzvm

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/vz"
)

// SupervisorFailedFile holds shard-init's reason for its own death, which a test turns into a fifo to hold the report's write.
const SupervisorFailedFile = supervisorFailedFile

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

// FailLinkClose gives the running sandbox id a link whose close returns err.
func (p *Provider) FailLinkClose(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.machines[id].link = failingLink{err: err}
}

type failingLink struct{ err error }

func (l failingLink) Close() error { return l.err }

// ForkSnapshot is the restore of a snapshot into a new sandbox, which the live fork runs on its capture.
func (p *Provider) ForkSnapshot(ctx context.Context, dir string, spec models.SandboxSpec) error {
	return p.forkSnapshot(ctx, dir, spec)
}

// CaptureCut is a fork cut after its capture and before it ran the source on, which a test cannot cut inside Fork.
func (p *Provider) CaptureCut(ctx context.Context, id, dir string) error {
	_, err := p.hold(ctx, id, dir)

	return err
}

// CaptureDir is where a fork stages its source's capture, in the fork's own state directory.
const CaptureDir = captureDir

// SetRedialGrace shortens the wait for the control stream a save's run dials again, which a test runs out on purpose.
func SetRedialGrace(grace time.Duration) (restore func()) {
	was := redialGrace
	redialGrace = grace

	return func() { redialGrace = was }
}

// HoldNextDir stops the next lookup of a sandbox's directory until release closes, and closes entered when that lookup begins.
func (p *Provider) HoldNextDir(entered chan<- struct{}, release <-chan struct{}) {
	dirs := p.cfg.Dirs
	var taken atomic.Bool
	p.cfg.Dirs = func(id string) (string, error) {
		if taken.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}

		return dirs(id)
	}
}
