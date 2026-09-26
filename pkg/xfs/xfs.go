// Package xfs drives mkfs.xfs and mount for one loopback image, the way the daemon provisions a reflink root.
package xfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Mkfs is the binary xfsprogs ships; a host without it cannot format an image.
const Mkfs = "mkfs.xfs"

// magic is the first four bytes of every xfs superblock.
const magic = "XFSB"

// stagingSuffix names the file a format works in until it succeeds.
const stagingSuffix = ".part"

// ErrNotLinux is what an image answers off Linux: fallocate, loop devices and mkfs.xfs live nowhere else.
var ErrNotLinux = errors.New("xfs: linux only")

// ErrNotImage is a file at the image path that mkfs.xfs never wrote; the daemon must not format over it.
var ErrNotImage = errors.New("not an xfs image")

// ErrFstabConflict is a line at the mount point that is not this loop mount; the next boot would mount that instead.
var ErrFstabConflict = errors.New("fstab already mounts the point from another source")

// Have reports whether mkfs.xfs is on PATH, and names the package when it is not.
func Have() error {
	if _, err := exec.LookPath(Mkfs); err != nil {
		return fmt.Errorf("%s is not on PATH; install xfsprogs", Mkfs)
	}

	return nil
}

// MakeImage reserves size bytes at path and formats them with reflink on; an image that exists is kept as it is.
func MakeImage(ctx context.Context, path string, size int64) error {
	formatted, err := IsImage(path)
	if err != nil {
		return err
	}
	if formatted {
		return nil
	}

	// The work lands under a staging name, so a crash or a failed mkfs never leaves an unformatted file at path.
	staging := path + stagingSuffix
	if err := os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", staging, err)
	}
	if err := reserve(staging, size); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, Mkfs, "-q", "-m", "reflink=1", staging).CombinedOutput(); err != nil {
		return errors.Join(fmt.Errorf("%s %s: %w: %s", Mkfs, staging, err, bytes.TrimSpace(out)), os.Remove(staging))
	}
	if err := os.Rename(staging, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", staging, path, err)
	}

	return nil
}

// Room is the bytes a new image at path can take: the free space beside it, plus the staging file of a crashed format, which MakeImage removes first.
func Room(path string) (int64, error) {
	return room(path)
}

// IsImage reports whether path holds an xfs superblock; a missing path is not an image, another file is an error.
func IsImage(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	head := make([]byte, len(magic))
	n, err := f.Read(head)
	if err != nil && n < len(magic) {
		return false, fmt.Errorf("%s: %w", path, ErrNotImage)
	}
	if string(head[:n]) != magic {
		return false, fmt.Errorf("%s: %w", path, ErrNotImage)
	}

	return true, nil
}

// Mount attaches the image over a loop device at point.
func Mount(ctx context.Context, image, point string) error {
	if out, err := exec.CommandContext(ctx, "mount", "-t", "xfs", "-o", "loop", image, point).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s at %s: %w: %s", image, point, err, bytes.TrimSpace(out))
	}

	return nil
}

// FstabPath is where the line goes; a test points it at a file of its own.
var FstabPath = "/etc/fstab"

// Fstab adds the line that mounts image at point on boot, once; a line that mounts point from anything else is a conflict.
func Fstab(image, point string) error {
	present, err := InFstab(image, point)
	if err != nil {
		return err
	}
	if present {
		return nil
	}

	f, err := os.OpenFile(FstabPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", FstabPath, err)
	}
	if _, err := fmt.Fprintf(f, "%s %s xfs loop 0 0\n", image, point); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", FstabPath, err), f.Close())
	}

	return f.Close()
}

// InFstab reports whether our line already mounts point, and refuses a line that mounts it from another source, type or without loop.
func InFstab(image, point string) (bool, error) {
	f, err := os.Open(FstabPath)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", FstabPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || fields[1] != point {
			continue
		}
		if len(fields) >= 4 && fields[0] == image && fields[2] == "xfs" && slices.Contains(strings.Split(fields[3], ","), "loop") {
			return true, nil
		}

		return false, fmt.Errorf("%s: %w: %q", FstabPath, ErrFstabConflict, strings.Join(fields, " "))
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", FstabPath, err)
	}

	return false, nil
}
