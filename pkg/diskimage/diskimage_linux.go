//go:build linux

// Package diskimage makes and mounts a sparse ext4 image, so a directory tree is bounded by a file's size.
package diskimage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/pkg/mountinfo"
)

// Create writes a sparse ext4 filesystem of size bytes at path, and leaves one that exists alone.
func Create(path string, size int64) error {
	if size <= 0 {
		return fmt.Errorf("a disk image needs a size in bytes, got %d", size)
	}

	// O_EXCL, so a second create never formats an image a mount already holds.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	// Truncate makes the file sparse: the host pays for a block when the guest first writes it.
	if err := errors.Join(f.Truncate(size), f.Close()); err != nil {
		return errors.Join(fmt.Errorf("size %s: %w", path, err), os.Remove(path))
	}

	// No reserved blocks, so the bound is what the guest can fill; lazy init, so mkfs writes almost nothing.
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-m", "0", "-E", "lazy_itable_init=1,lazy_journal_init=1", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Join(fmt.Errorf("mkfs.ext4 %s: %w: %s", path, err, out), os.Remove(path))
	}

	return nil
}

// Mount attaches image at dir over a loop device the kernel frees with the mount. It is safe to call twice.
func Mount(image, dir string) error {
	mounted, err := Mounted(image, dir)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	// noinit_itable: the kernel would otherwise zero every inode table in the background, a host block each.
	cmd := exec.Command("mount", "-t", "ext4", "-o", "loop,noinit_itable", image, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s on %s: %w: %s", image, dir, err, out)
	}

	return nil
}

// Unmount detaches dir. MNT_DETACH, because a file the guest left open must not make a stop fail.
func Unmount(image, dir string) error {
	mounted, err := Mounted(image, dir)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}

	if err := syscall.Unmount(dir, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", dir, err)
	}

	return nil
}

// Mounted asks the kernel whether dir holds image, and refuses a mount at dir that is something else.
func Mounted(image, dir string) (bool, error) {
	m, found, err := mountinfo.At(dir)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	backing, err := backingFile(m.Source)
	if err != nil {
		return false, err
	}
	if m.FSType != "ext4" || backing != image {
		return false, fmt.Errorf("%s already holds a %s mount of %s that is not %s", dir, m.FSType, m.Source, image)
	}

	return true, nil
}

// backingFile is the file behind a loop device, which is how the kernel names the image a mount came from.
func backingFile(source string) (string, error) {
	if !strings.HasPrefix(source, "/dev/loop") {
		return "", nil
	}

	path := filepath.Join("/sys/block", filepath.Base(source), "loop", "backing_file")
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	// An image removed under its mount is reported with a suffix, and is still the image.
	return strings.TrimSuffix(strings.TrimSpace(string(blob)), " (deleted)"), nil
}
