package runc

import (
	"github.com/presmihaylov/shard/models"
	runccli "github.com/presmihaylov/shard/pkg/runc"
	"github.com/presmihaylov/shard/services/bundle"
)

// SetCgroupRoot points a provider at a cgroup tree a test owns, so a unit test reads why a sandbox died without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// ExecOptions is the process an exec hands runc, reachable without a running sandbox.
func ExecOptions(b bundle.Bundle, spec models.ExecSpec) (runccli.ExecOptions, error) {
	return execOptions(b, spec)
}

// NotStarted is the refusal an exec gets when its launch never took.
var NotStarted = notStarted
