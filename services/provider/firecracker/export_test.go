package firecracker

import "github.com/presmihaylov/shard/models"

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
