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

// EndCut resumes a read that saw pid in a state a cut daemon leaves, which a test cannot pause inside Status.
func (p *Provider) EndCut(id string, client *fcapi.Client, pid int) error {
	return p.endCut(id, client, pid)
}

// Install is a pause cut after its install and before it ended the vmm, which a test cannot cut inside Pause.
func (p *Provider) Install(ctx context.Context, id, dir string) error {
	_, err := p.install(ctx, id, dir)

	return err
}
