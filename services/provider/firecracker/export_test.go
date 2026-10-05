package firecracker

import (
	"context"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
)

// BoundVMM makes the host cgroup a boot spawns the vmm into. A test cannot reach it through Create,
// which needs /dev/kvm and root, so it reaches it here over a directory it owns.
func BoundVMM(root, id string, r models.Resources) (string, error) {
	return boundVMM(root, id, r)
}

// SetCgroupRoot points a provider at a directory a test owns, and an empty one at no hierarchy at
// all, which is what a test host that is not Linux has. BoundVMM is what covers the bound itself.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// Spawning marks a sandbox as one this provider brings a vmm up for, which a test cannot hold open through Create.
func (p *Provider) Spawning(id string) (done func()) {
	return p.spawn(id)
}

// EndJudged resumes a read that judged pid dead weight, which a test cannot pause inside Status.
func (p *Provider) EndJudged(id string, client *fcapi.Client, pid int, jail string) error {
	return p.endJudged(id, client, pid, jail)
}

// RenumberSilent points the pid a silent vmm reads under at another process, which is how a pid the kernel reused looks from here.
func (p *Provider) RenumberSilent(id string, pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, held := p.machines[id]; held {
		m.pid = pid

		return
	}
	p.unadopted[id].pid = pid
}

// EndUnattachedAs ends a vmm whose guest does not attach after its pid reads as pid, which is how a pid the kernel reused between the attach and the kill looks from here.
func (p *Provider) EndUnattachedAs(ctx context.Context, id string, pid int) error {
	dir, r, err := p.open(id)
	if err != nil {
		return err
	}
	socket, vsock := r.sockets(dir)
	client, info, err := fcapi.Adopt(ctx, socket, vsock)
	if err != nil {
		return err
	}
	m, err := p.attach(ctx, id, dir, r.Jail, client, info, adoptBound)
	if m == nil {
		return fmt.Errorf("the attach handed back no machine: %w", err)
	}
	m.pid = pid

	return p.endUnattached(ctx, m, err)
}

// SetOwners stands in for the chown and the tap's owner, which need root; a test runs as a user who can give a file to nobody.
func (p *Provider) SetOwners(chown func(name string, uid, gid int) error, ownTap func(namespace, name string, uid, gid int) error) {
	p.chown, p.ownTap = chown, ownTap
}

// Install is a pause cut after its install and before it ended the vmm, which a test cannot cut inside Pause.
func (p *Provider) Install(ctx context.Context, id, dir string) error {
	_, err := p.install(ctx, id, dir)

	return err
}

// RestoringFile is the marker a cut fork leaves, which a test writes to stand in for a restore the daemon died inside.
const RestoringFile = restoringFile

// SupervisorFailedFile holds shard-init's reason for its own death, which a test turns into a fifo to hold the report's write.
const SupervisorFailedFile = supervisorFailedFile

// ReseedFile is the marker a restore keeps until its guest is reseeded, which a test writes to stand in for a daemon cut before the reseed.
const ReseedFile = reseedFile

// ForkCheckpoint is the restore of a checkpoint into a new sandbox, which the live fork runs on its capture.
func (p *Provider) ForkCheckpoint(ctx context.Context, dir string, spec models.SandboxSpec) error {
	return p.forkCheckpoint(ctx, dir, spec)
}

// CaptureCut is a fork cut after its capture and before it ran the source on, which a test cannot cut inside Fork.
func (p *Provider) CaptureCut(ctx context.Context, id, dir string) error {
	_, err := p.hold(ctx, id, dir)

	return err
}

// CaptureFile is the marker a capture keeps on its source until the source runs again.
const CaptureFile = captureFile

// CaptureDir is where a fork stages its source's capture, in the fork's own state directory.
const CaptureDir = captureDir

// SetRedialGrace shortens the wait for the control stream a snapshot's run dials again, which a test runs out on purpose.
func SetRedialGrace(grace time.Duration) (restore func()) {
	was := redialGrace
	redialGrace = grace

	return func() { redialGrace = was }
}

// Holds says whether the provider keeps a machine for id, which a verb that failed must not leave behind.
func (p *Provider) Holds(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, held := p.machines[id]

	return held
}

// RestoreFiles swaps the checkpoint's overlay under the sandbox, which a test drives directly to prove a failed copy keeps the live overlay.
func RestoreFiles(dir, stateDir string) error { return restoreFiles(dir, stateDir) }
