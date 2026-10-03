package firecracker

import (
	"context"

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
	p.unadopted[id].pid = pid
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
