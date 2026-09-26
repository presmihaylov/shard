package bundle

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
)

// OverlayDiskFile names the writable disk a microVM sandbox mounts over its read-only image; the guest lays its upper and work directories on it.
const OverlayDiskFile = "overlay.raw"

// WriteOverlayDisk lays an empty ext4 image down at dst, grown to the bound of r, so every write of the sandbox lands on it and stops there.
func WriteOverlayDisk(dst string, r models.Resources) error {
	var empty bytes.Buffer
	if err := tar.NewWriter(&empty).Close(); err != nil {
		return fmt.Errorf("write an empty tar: %w", err)
	}

	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if err := lay(f, &empty); err != nil {
		return errors.Join(err, os.Remove(dst))
	}
	if err := ext4.Grow(dst, DiskBytes(r)); err != nil {
		return errors.Join(fmt.Errorf("grow %s to the %d MiB bound: %w", dst, DiskBound(r), err), os.Remove(dst))
	}

	return nil
}

func lay(f *os.File, tree io.Reader) (err error) {
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", f.Name(), cerr))
		}
	}()
	if err := ext4.Write(tree, f); err != nil {
		return fmt.Errorf("write %s: %w", f.Name(), err)
	}

	return nil
}
