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
	shared, err = cloneOrCopy(base, dst)
	if err != nil {
		return false, err
	}

	if err := ext4.Grow(dst, DiskBytes(r)); err != nil {
		return false, errors.Join(fmt.Errorf("grow %s to the %d MiB bound: %w", dst, DiskBound(r), err), os.Remove(dst))
	}

	return shared, nil
}

func cloneOrCopy(base, dst string) (bool, error) {
	err := clonefile(base, dst)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		return false, fmt.Errorf("clone %s: %w", base, err)
	}

	return false, copyFile(base, dst)
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", dst, cerr)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}

	return nil
}
