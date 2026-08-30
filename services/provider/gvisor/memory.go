package gvisor

import (
	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// The sentry is one host process and it holds the guest's memory, so the host cgroup charges the
// sentry's own working set against the sandbox's bound. Measured on the devbox it is flat at 26 to
// 31 MiB, whatever the bound is and whatever the guest holds, so the headroom is a constant.
const sentryOverheadMiB = 32

// The gap between the throttle and the ceiling. It is what a guest may hold past its own bound
// before the host OOM killer ends the sandbox, and nothing but a runaway ever reaches it.
const runawayMarginMiB = 32

// MinimumMemoryMiB is the smallest bound the sentry survives. runsc creates the cgroup with the bare
// bound and boots the sentry inside it, so a bound under the sentry's own cost kills the create.
// Measured on the devbox: 16 MiB is killed, 24 MiB boots and holds 20 MiB with an idle guest.
const MinimumMemoryMiB = 64

const bytesPerMiB = 1024 * 1024

// MemoryThrottle is memory.high: the point the kernel starts reclaiming against, which is where the
// guest has used its whole bound. Nothing is killed here, it is only slowed down.
func MemoryThrottle(r models.Resources) int64 {
	if bundle.MemoryBound(r) == 0 {
		return 0
	}

	return (r.MemoryMiB + sentryOverheadMiB) * bytesPerMiB
}

// MemoryCeiling is memory.max: the point the host OOM killer answers, which ends the sandbox. It
// sits above the throttle so a guest that merely reaches its bound is slowed and never killed.
func MemoryCeiling(r models.Resources) int64 {
	if bundle.MemoryBound(r) == 0 {
		return 0
	}

	return (r.MemoryMiB + sentryOverheadMiB + runawayMarginMiB) * bytesPerMiB
}
