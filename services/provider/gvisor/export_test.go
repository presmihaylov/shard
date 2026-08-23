package gvisor

import "github.com/presmihaylov/shard/models"

// BoundMemory drives what create does to the cgroup runsc just made. A test cannot reach it through
// Create, which needs root and a real rootfs mount, so it reaches it here over a directory it owns.
func BoundMemory(root string, spec models.SandboxSpec) error {
	return boundMemory(root, spec)
}
