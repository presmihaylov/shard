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
	shared, err = CloneFile(base, dst)
	if err != nil {
		return false, err
	}

	if err := ext4.Grow(dst, DiskBytes(r)); err != nil {
		return false, errors.Join(fmt.Errorf("grow %s to the %d MiB bound: %w", dst, DiskBound(r), err), os.Remove(dst))
	}

	return shared, nil
}

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
