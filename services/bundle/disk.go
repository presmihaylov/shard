package bundle

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/presmihaylov/shard/models"
)

// DefaultDiskMiB is the disk a sandbox gets when it names no bound: 10 GiB, sparse, so an unwritten sandbox costs the host nothing.
const DefaultDiskMiB = 10240

// diskAnnotation records the bound in config.json, the one file a start after a stop reads a bundle back from.
const diskAnnotation = "dev.shard.disk-mib"

// diskLocks holds one mutex per disk image: two clones of one stopped source race Mounted against Unmount without it (SHARD-251).
var diskLocks = struct {
	sync.Mutex
	byImage map[string]*sync.Mutex
}{byImage: map[string]*sync.Mutex{}}

// lockDisk takes the mutex of this bundle's disk image and returns its release.
func (b Bundle) lockDisk() func() {
	diskLocks.Lock()
	lock, ok := diskLocks.byImage[b.Image]
	if !ok {
		lock = &sync.Mutex{}
		diskLocks.byImage[b.Image] = lock
	}
	diskLocks.Unlock()

	lock.Lock()

	return lock.Unlock
}

// DiskBound is the size of a sandbox's disk in MiB, never 0: its writable layer and its /tmp are host files.
func DiskBound(r models.Resources) int64 {
	if r.DiskMiB <= 0 {
		return DefaultDiskMiB
	}

	return r.DiskMiB
}

// DiskBytes is the image size the bound asks for.
func DiskBytes(r models.Resources) int64 {
	return DiskBound(r) * bytesPerMiB
}

// diskOf reads the bound back from config.json. A file from before the bound names none, which is the default.
func diskOf(annotations map[string]string) (int64, error) {
	value, ok := annotations[diskAnnotation]
	if !ok {
		return 0, nil
	}

	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the %s annotation is not a count of MiB: %w", diskAnnotation, err)
	}

	return n, nil
}
