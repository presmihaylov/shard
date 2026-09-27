package gvisor

import (
	"context"
	"os"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
)

// BoundMemory drives what create does to the cgroup runsc just made. A test cannot reach it through
// Create, which needs root and a real rootfs mount, so it reaches it here over a directory it owns.
func BoundMemory(root string, spec models.SandboxSpec) error {
	return boundMemory(root, spec)
}

// BoundPids drives the pids cap create writes on the cgroup, reachable without runsc or root.
func BoundPids(root, id string) error {
	return boundPids(root, id)
}

// SetCgroupRoot points a provider at a directory a test owns, so the reason a dead sandbox died can
// be read without a real cgroup hierarchy and without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// SetProcRoot points a provider at a directory that stands in for /proc, so a reclaim reads command lines a test wrote.
func (p *Provider) SetProcRoot(root string) {
	p.procRoot = root
}

// SetKill replaces the SIGKILL a reclaim sends, so a test records the pids instead of killing anything.
func (p *Provider) SetKill(kill func(pid int) error) {
	p.killProcess = kill
}

// Sweep is the kill Remove runs on what a cut-short create left in the cgroup, reachable without runsc.
func (p *Provider) Sweep(ctx context.Context, id string) error {
	return p.sweep(ctx, id)
}

// BringUp is the launch create, fork and resume share, with the runsc verb a test scripts.
func (p *Provider) BringUp(ctx context.Context, spec models.SandboxSpec, exitFile string, up func(out, exit *os.File) error) error {
	return p.bringUp(ctx, spec, exitFile, up)
}

// RemoveCgroup is the sweep Remove runs after runsc delete, reachable without runsc.
func RemoveCgroup(root, id string) error {
	return cgroup.Remove(cgroupDir(root, id))
}

// ZombieStat is the /proc stat parse Status runs on a live answer, reachable with a fixture line.
func ZombieStat(stat string) bool {
	return zombieStat(stat)
}

// Vanished is the classification zombie runs on a read of /proc that failed, reachable with an error.
func Vanished(err error) bool {
	return vanished(err)
}
