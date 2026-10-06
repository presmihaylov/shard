package sysbox

import (
	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/runc"
	"github.com/presmihaylov/shard/services/bundle"
)

// SetCgroupRoot points a provider at a cgroup tree a test owns, so a unit test reads why a sandbox died, and a reopen confirms PID 1, without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// SetProcRoot points a provider at a directory that stands in for /proc, so a reopen finds the fd 0 a test placed.
func (p *Provider) SetProcRoot(root string) {
	p.procRoot = root
}

// ExecOptions is the process an exec hands sysbox-runc, with guest standing in for the root PID 1 sees.
func ExecOptions(b bundle.Bundle, guest string, spec models.ExecSpec) (runc.ExecOptions, error) {
	return execOptions(b, guest, spec)
}

// NotStarted is the refusal an exec gets when its launch never took.
var NotStarted = notStarted
