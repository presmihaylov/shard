package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
)

// GrowDisk grows the ext4 disk at path to the bound of r on a clone, then renames the clone over it, so a failure leaves the disk as it was.
func GrowDisk(path string, r models.Resources, clone func(from, to string) error) error {
	return admitDisk(path, DiskBytes(r), func() error {
		staged := path + ".grow"
		if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear the staged disk: %w", err)
		}
		if err := clone(path, staged); err != nil {
			return fmt.Errorf("stage the disk to grow: %w", err)
		}
		if err := ext4.Grow(staged, DiskBytes(r)); err != nil {
			return errors.Join(growRefusal(err, DiskBound(r)), os.Remove(staged))
		}
		if err := os.Rename(staged, path); err != nil {
			return errors.Join(fmt.Errorf("swap the grown disk in: %w", err), os.Remove(staged))
		}

		return nil
	})
}

// growRefusal names what works when ext4 cannot grow a stopped sandbox's disk.
func growRefusal(err error, mib int64) error {
	if errors.Is(err, ext4.ErrNeedsRecovery) {
		return &BoundError{Fix: fmt.Sprintf("the sandbox's disk was not stopped clean, so it cannot grow to %d MiB; start the sandbox, stop it, and resize again", mib), Err: err}
	}
	if taken, ok := errors.AsType[*ext4.DescriptorTakenError](err); ok {
		most := taken.Max / bytesPerMiB

		return &BoundError{Fix: fmt.Sprintf("the sandbox's disk grows to at most %d MiB, as the guest took the room a larger one needs; set resources.disk_mib to %d MiB or less", most, most), Err: err}
	}

	return fmt.Errorf("grow the disk to the %d MiB bound: %w", mib, err)
}

// Resize grows the disk, then rewrites the bounds in config.json, which the next start creates the container from.
func (b Bundle) Resize(res models.Resources) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}
	disk, err := diskOf(spec.Annotations)
	if err != nil {
		return fmt.Errorf("read the disk bound back from %s: %w", b.configPath(), err)
	}
	if spec.Linux == nil || spec.Linux.Resources == nil {
		return fmt.Errorf("%s names no resources to resize", b.configPath())
	}
	have, want := DiskBound(models.Resources{DiskMiB: disk}), DiskBound(res)
	if want < have {
		return fmt.Errorf("the disk of %s is %d MiB and does not shrink to %d MiB", b.Dir, have, want)
	}
	if want > have {
		if err := b.growDisk(res); err != nil {
			return err
		}
	}
	// The bound is written after the grow, so a refused grow leaves the disk bound the sandbox had.
	if spec.Annotations == nil {
		spec.Annotations = map[string]string{}
	}
	spec.Annotations[diskAnnotation] = strconv.FormatInt(want, 10)

	bounds := resources(res)
	spec.Linux.Resources.Memory = bounds.Memory
	spec.Linux.Resources.CPU = bounds.CPU
	// /dev/shm is sized off the memory bound, so a smaller bound shrinks it too.
	for i, m := range spec.Mounts {
		if m.Destination == "/dev/shm" {
			resizeTmpfs(spec.Mounts[i].Options, shmSize(res))
		}
	}

	return b.writeSpec(spec)
}

// resizeTmpfs rewrites the size option of a tmpfs mount in place.
func resizeTmpfs(options []string, size int64) {
	for i, opt := range options {
		if strings.HasPrefix(opt, "size=") {
			options[i] = tmpfsSize(size)
		}
	}
}
