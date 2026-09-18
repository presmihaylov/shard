package bundle

import "github.com/presmihaylov/shard/models"

// DefaultPidsMax is the pids.max a sandbox gets when it names no bound. It runs systemd, Docker and
// nested containers with headroom, yet stops a fork bomb far below host PID exhaustion.
const DefaultPidsMax = 4096

// PidsBound is the pids.max a sandbox cgroup gets: the operator's bound, or DefaultPidsMax for none.
// A sandbox is never unbounded on PIDs, because on Sysbox its processes are host processes.
func PidsBound(r models.Resources) int64 {
	if r.PidsMax <= 0 {
		return DefaultPidsMax
	}

	return r.PidsMax
}
