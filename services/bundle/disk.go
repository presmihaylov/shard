package bundle

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
)

// DefaultDiskMiB is the disk a sandbox gets when it names no bound: 10 GiB, sparse, so an unwritten sandbox costs the host nothing.
const DefaultDiskMiB = 10240

// diskAnnotation records the bound in config.json, the one file a start after a stop reads a bundle back from.
const diskAnnotation = "dev.shard.disk-mib"

// diskLocks holds one mutex per disk image in use: two snapshots of one stopped source race Mounted against Unmount without it (SHARD-251).
var diskLocks = struct {
	sync.Mutex
	byImage map[string]*diskLock
}{byImage: map[string]*diskLock{}}

// diskLock counts its holders and waiters, so the entry leaves the table with the last of them.
type diskLock struct {
	sync.Mutex
	users int
}

// lockDisk takes the mutex of this bundle's disk image and returns its release.
func (b Bundle) lockDisk() func() {
	diskLocks.Lock()
	lock, ok := diskLocks.byImage[b.Image]
	if !ok {
		lock = &diskLock{}
		diskLocks.byImage[b.Image] = lock
	}
	lock.users++
	diskLocks.Unlock()

	lock.Lock()

	return func() {
		lock.Unlock()
		diskLocks.Lock()
		lock.users--
		if lock.users == 0 {
			delete(diskLocks.byImage, b.Image)
		}
		diskLocks.Unlock()
	}
}

// DiskBound is the size of a sandbox's disk in MiB, never 0: its writable layer and its /tmp are host files.
func DiskBound(r models.Resources) int64 {
	if r.DiskMiB <= 0 {
		return DefaultDiskMiB
	}

	return r.DiskMiB
}

// CheckGrowBound refuses a bound ext4.Grow cannot reach from a smaller image, and names the nearest bounds it can.
func CheckGrowBound(mib int64) error {
	if ext4.LastGroupFits(mib * bytesPerMiB) {
		return nil
	}
	below, above := mib-1, mib+1
	for !ext4.LastGroupFits(below * bytesPerMiB) {
		below--
	}
	for !ext4.LastGroupFits(above * bytesPerMiB) {
		above++
	}

	return fmt.Errorf("a %d MiB disk ends on a block group too small for its own metadata; set resources.disk_mib to %d MiB or %d MiB", mib, below, above)
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
