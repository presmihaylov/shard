package bundle

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
)

// CloneRootDisk gives one sandbox its own copy of the image disk at dst, grown to its bound; shared says the blocks are an APFS clone.
func CloneRootDisk(base, dst string, r models.Resources) (shared bool, err error) {
	st, err := os.Stat(base)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", base, err)
	}
	// The image size is known only after the pull, so the provider cannot refuse this bound up front.
	if need := ceilMiB(st.Size()); st.Size() > DiskBytes(r) {
		return false, &BoundError{Fix: fmt.Sprintf("the image takes a %d MiB disk, more than the %d MiB disk bound; set resources.disk_mib to %d MiB or more", need, DiskBound(r), need)}
	}

	err = admitDisk(dst, DiskBytes(r), func() error {
		shared, err = CloneFile(base, dst)
		if err != nil {
			return err
		}
		if err := ext4.Grow(dst, DiskBytes(r)); err != nil {
			return errors.Join(fmt.Errorf("grow %s to the %d MiB bound: %w", dst, DiskBound(r), err), os.Remove(dst))
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	return shared, nil
}

// GrowSeed lays a snapshot's disk down at dst with copy, then grows it to the bound of r; the service refused a bound under the disk's own size.
func GrowSeed(dst string, r models.Resources, copy func() error) error {
	return admitDisk(dst, DiskBytes(r), func() error {
		if err := copy(); err != nil {
			return err
		}
		if err := ext4.Grow(dst, DiskBytes(r)); err != nil {
			return errors.Join(seedRefusal(err, DiskBound(r)), os.Remove(dst))
		}

		return nil
	})
}

// seedRefusal names the resources.disk_mib that works when ext4 cannot grow a snapshot's disk.
func seedRefusal(err error, mib int64) error {
	if errors.Is(err, ext4.ErrNeedsRecovery) {
		return &BoundError{Fix: fmt.Sprintf("the snapshot's disk was not stopped clean, so it cannot grow to %d MiB; omit resources.disk_mib, or start the sandbox it came from, let its entrypoint exit or end it with shard exec, then stop it and snapshot it again", mib), Err: err}
	}
	var taken *ext4.DescriptorTakenError
	if errors.As(err, &taken) {
		most := taken.Max / bytesPerMiB

		return &BoundError{Fix: fmt.Sprintf("the snapshot's disk grows to at most %d MiB, as a mount took the room a larger one needs; set resources.disk_mib to %d MiB or less", most, most), Err: err}
	}

	return fmt.Errorf("grow the snapshot's disk to the %d MiB bound: %w", mib, err)
}

// BoundError is a disk bound the image or the snapshot cannot take; Fix is what a public route answers, and Err stays in the daemon log.
type BoundError struct {
	Fix string
	Err error
}

func (e *BoundError) Error() string {
	if e.Err == nil {
		return e.Fix
	}

	return e.Fix + ": " + e.Err.Error()
}

func (e *BoundError) Unwrap() error { return e.Err }

func (e *BoundError) Public() string { return e.Fix }

// CloneFile copies base to dst as it is, sharing the blocks where the filesystem can; shared says it did.
func CloneFile(base, dst string) (bool, error) {
	err := clonefile(base, dst)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		return false, fmt.Errorf("clone %s: %w", base, err)
	}

	return false, copyFile(base, dst)
}

// Reflink copies base to dst by sharing its blocks, and refuses where the filesystem cannot: a clone that copies every byte is not a clone.
func Reflink(base, dst string) error {
	if err := clonefile(base, dst); err != nil {
		return fmt.Errorf("reflink %s: %w", base, err)
	}

	return nil
}

func copyFile(src, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	// A half-written copy is removed once both files are closed, so a retry finds the name free.
	if err := fill(out, src); err != nil {
		return errors.Join(err, os.Remove(dst))
	}

	return nil
}

func fill(out *os.File, src string) (err error) {
	defer func() {
		if cerr := out.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", out.Name(), cerr))
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() {
		if cerr := in.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", src, cerr))
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, out.Name(), err)
	}

	return nil
}

func ceilMiB(n int64) int64 {
	return (n + bytesPerMiB - 1) / bytesPerMiB
}
