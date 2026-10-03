package firecracker

import (
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

// EndUnloaded resumes a read that saw pid answer "Not started", which a test cannot pause inside Status.
func (p *Provider) EndUnloaded(id string, client *fcapi.Client, pid int) error {
	return p.endUnloaded(id, client, pid)
}

// RestoringFile is the marker a cut fork leaves, which a test writes to stand in for a restore the daemon died inside.
const RestoringFile = restoringFile

// SupervisorFailedFile holds shard-init's reason for its own death, which a test turns into a fifo to hold the report's write.
const SupervisorFailedFile = supervisorFailedFile

// ReseedFile is the marker a restore keeps until its guest is reseeded, which a test writes to stand in for a daemon cut before the reseed.
const ReseedFile = reseedFile
