package sandbox

import "github.com/presmihaylov/shard/models"

// trigger is what starts a sandbox's processes again with it.
type trigger int

const (
	// operatorStart is shard start.
	operatorStart trigger = iota
	// daemonStart is a daemon that comes up, after its own restart or the host's, or that starts a sandbox again after an OOM kill.
	daemonStart
)

// launchable decides, by Docker's rules, whether a start of the sandbox starts this process again.
func launchable(p models.Process, sb models.Sandbox, by trigger) bool {
	switch p.Restart.Policy {
	case models.RestartAlways:
		return true
	case models.RestartUnlessStopped:
		return !p.Killed && (by == operatorStart || !sb.StoppedByOperator)
	}

	return false
}
