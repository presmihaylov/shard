package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// ResizeRequest names the bounds a stopped sandbox's next start runs under.
type ResizeRequest struct {
	Resources ResizeResources `json:"resources"`
}

// ResizeResources is a create's bounds where an absent one keeps the sandbox's own.
type ResizeResources struct {
	MemoryMiB *int64 `json:"memory_mib,omitempty" minimum:"0" maximum:"16777216" doc:"The memory bound in MiB. 0 is 512 on the firecracker and vz providers, and no bound on gvisor, runc and sysbox."`
	VCPUs     *int   `json:"vcpus,omitempty" minimum:"0" doc:"The vCPU bound. 0 is every CPU of the host."`
	DiskMiB   *int64 `json:"disk_mib,omitempty" minimum:"0" maximum:"16777088" doc:"The disk bound in MiB, which only grows, and only on a provider whose capabilities say disk_grow. 0 is 10240."`
	SwapMiB   *int64 `json:"swap_mib,omitempty" minimum:"0" maximum:"16777088" doc:"The swap file in MiB, which counts against disk_mib. 0 is none; gvisor, runc and sysbox refuse any other."`
}

func (r ResizeResources) empty() bool {
	return r.MemoryMiB == nil && r.VCPUs == nil && r.DiskMiB == nil && r.SwapMiB == nil
}

// over lays the request over the sandbox's own bounds, as the create request it would have been.
func (r ResizeResources) over(res models.Resources) ResourceRequest {
	out := ResourceRequest{MemoryMiB: new(res.MemoryMiB), VCPUs: res.VCPUs, DiskMiB: res.DiskMiB, SwapMiB: new(res.SwapMiB)}
	if r.MemoryMiB != nil {
		out.MemoryMiB = r.MemoryMiB
	}
	if r.VCPUs != nil {
		out.VCPUs = *r.VCPUs
	}
	if r.DiskMiB != nil {
		out.DiskMiB = *r.DiskMiB
	}
	if r.SwapMiB != nil {
		out.SwapMiB = r.SwapMiB
	}

	return out
}

// Resize changes the bounds of a stopped sandbox, which its next start runs under. A disk only grows.
func (s *Service) Resize(ctx context.Context, ref string, req ResizeRequest) (models.Sandbox, error) {
	if req.Resources.empty() {
		return models.Sandbox{}, &RequestError{Err: errors.New("the resize names no bound; set resources.memory_mib, resources.vcpus, resources.disk_mib or resources.swap_mib")}
	}

	// A created VM is already booted, so only a stopped sandbox has no guest that holds its bounds.
	id, sb, unlock, err := s.holdIn(ctx, ref, "resize takes a stopped sandbox: stop it first", models.StateStopped)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	merged := req.Resources.over(sb.Resources)
	if err := validBounds(merged); err != nil {
		return models.Sandbox{}, err
	}
	res, err := s.checkBounds(s.resources(merged))
	if err != nil {
		return models.Sandbox{}, err
	}

	was := sb.Resources
	was.DiskMiB = bundle.DiskBound(was)
	if res.DiskMiB < was.DiskMiB {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("resources.disk_mib is %d MiB, smaller than the %d MiB disk of sandbox %s, and a disk only grows; omit it or set it to %d MiB or more", res.DiskMiB, was.DiskMiB, nameOf(id, sb), was.DiskMiB)}
	}
	if res.DiskMiB > was.DiskMiB {
		if err := requireVerb(s.cfg.Provider, models.VerbDiskGrow); err != nil {
			return models.Sandbox{}, err
		}
	}
	if res == was {
		return sb, nil
	}

	env, err := s.environment(id)
	if err != nil {
		return models.Sandbox{}, err
	}
	// The substrate goes first, as its bounds are the ones a start runs under; a retry finishes a resize cut between the two.
	if err := env.Resize(res); err != nil {
		return models.Sandbox{}, diskRefused(err)
	}
	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Resources = res

		return nil
	})
	if err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}
