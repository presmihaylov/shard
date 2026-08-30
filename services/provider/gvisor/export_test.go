package gvisor

import (
	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
)

// BoundMemory drives what create does to the cgroup runsc just made. A test cannot reach it through
// Create, which needs root and a real rootfs mount, so it reaches it here over a directory it owns.
func BoundMemory(root string, spec models.SandboxSpec) error {
	return boundMemory(root, spec)
}

// SetCgroupRoot points a provider at a directory a test owns, so the reason a dead sandbox died can
// be read without a real cgroup hierarchy and without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// RemoveCgroup is the sweep Remove runs after runsc delete, reachable without runsc.
func RemoveCgroup(root, id string) error {
	return cgroup.Remove(cgroupDir(root, id))
}
