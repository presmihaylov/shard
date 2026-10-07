package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/mountinfo"
)

// swapFile sits at the top of the disk the root writes to: beside the overlay's upper and work, or in the root of a single disk.
const swapFile = ".shard-swap"

// makeSwap swaps onto a new file in dir; the old one goes first, since a forced stop never ran the swapoff that removes it.
func makeSwap(dir string, mib int64) error {
	path := filepath.Join(dir, swapFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the swap file of the last boot: %w", err)
	}
	if mib == 0 {
		return nil
	}
	if err := writeSwapFile(path, mib<<20); err != nil {
		return fmt.Errorf("make the %d MiB swap file, which needs that much free on the disk beside the image: %w", mib, err)
	}
	if err := swapCall(unix.SYS_SWAPON, path); err != nil {
		return fmt.Errorf("swapon %s: %w", path, err)
	}

	return nil
}

// writeSwapFile allocates the file whole, since swap writes to the disk under the filesystem and a hole has no block to land on.
func writeSwapFile(path string, size int64) error {
	header, err := swapHeader(size, os.Getpagesize())
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		return errors.Join(fmt.Errorf("allocate %d bytes: %w", size, err), f.Close(), os.Remove(path))
	}
	if _, err := f.WriteAt(header, 0); err != nil {
		return errors.Join(fmt.Errorf("write the swap header: %w", err), f.Close(), os.Remove(path))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync the swap file: %w", err), f.Close(), os.Remove(path))
	}

	return f.Close()
}

// dropSwap kills the guest's processes first, so swapoff reads none of their pages back in.
func dropSwap(bound, root *os.File) error {
	if root == nil {
		return nil
	}
	if err := emptyBound(bound); err != nil {
		return err
	}
	if err := dropShmem(); err != nil {
		return err
	}
	// The root disk has no path of its own once the overlay is on /, but PID 1 holds it open.
	if err := swapCall(unix.SYS_SWAPOFF, fmt.Sprintf("/proc/self/fd/%d/%s", root.Fd(), swapFile)); err != nil {
		return fmt.Errorf("swapoff the swap file: %w", err)
	}
	if err := unix.Unlinkat(int(root.Fd()), swapFile, 0); err != nil {
		return fmt.Errorf("remove the swap file: %w", err)
	}

	return nil
}

// dropShmem frees the tmpfs files and shared memory segments the dead processes left, or swapoff reads them back into a VM that may not hold them.
func dropShmem() error {
	mounts, err := mountinfo.All()
	if err != nil {
		return err
	}
	var points []string
	for _, m := range mounts {
		if m.FSType == "tmpfs" {
			points = append(points, m.Point)
		}
	}
	// The deepest first, since a detached tmpfs takes the path to the mounts under it.
	slices.SortStableFunc(points, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	for _, point := range points {
		if err := unix.Unmount(point, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("detach the tmpfs at %s: %w", point, err)
		}
	}
	// Setting it destroys every segment no process attaches, and no process is left.
	if err := os.WriteFile("/proc/sys/kernel/shm_rmid_forced", []byte("1"), 0); err != nil {
		return fmt.Errorf("destroy the shared memory segments: %w", err)
	}

	return nil
}

// emptyBound kills every process in the sandbox cgroup and returns once none is left.
func emptyBound(bound *os.File) error {
	if bound == nil {
		return nil
	}
	if err := writeAt(bound, "cgroup.kill", "1"); err != nil {
		return fmt.Errorf("kill the sandbox cgroup: %w", err)
	}
	deadline := time.Now().Add(freezeWait)
	for {
		events, err := readAt(bound, "cgroup.events")
		if err != nil {
			return fmt.Errorf("read the sandbox cgroup events: %w", err)
		}
		if slices.Contains(strings.Split(events, "\n"), "populated 0") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the sandbox cgroup still held a process %s after the kill", freezeWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// swapCall is swapon with no flags, or swapoff, which x/sys/unix does not wrap.
func swapCall(trap uintptr, path string) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	if _, _, errno := unix.Syscall(trap, uintptr(unsafe.Pointer(p)), 0, 0); errno != 0 { //nolint:gosec // the syscall takes a C string
		return errno
	}

	return nil
}
