package bundle

import "github.com/presmihaylov/shard/models"

const bytesPerMiB = 1024 * 1024

// MemoryBound is what the OCI spec carries, and runsc gives the sentry exactly this as its budget,
// so it is the MemTotal the guest reads. Any headroom over it is the host's business, not the guest's.
func MemoryBound(r models.Resources) int64 {
	if r.MemoryMiB <= 0 {
		return 0
	}

	return r.MemoryMiB * bytesPerMiB
}
