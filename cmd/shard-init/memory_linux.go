package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/cgroup"
)

// cgroupRoot is the cgroup2 mount; after the re-exec it shows the sandbox cgroup as the root of the guest's own namespace.
const cgroupRoot = "/sys/fs/cgroup"

// memoryHeadroom is what the bound leaves the kernel and shard-init, so the cgroup's OOM killer runs before the VM's own.
const memoryHeadroom int64 = 32 << 20

// initCgroup is PID 1's own, beside the sandbox cgroup: a freeze of the guest's processes leaves the supervisor running to answer.
const initCgroup = "init"

// The kernel spares global init, so children need no inherited OOM exemption.
func boundMemory() error {
	if err := cgroup.Delegate(cgroupRoot, "memory"); err != nil {
		return fmt.Errorf("enable the memory controller: %w", err)
	}
	if err := cgroup.Ensure(filepath.Join(cgroupRoot, initCgroup)); err != nil {
		return err
	}
	dir := filepath.Join(cgroupRoot, "sandbox")
	if err := cgroup.Ensure(dir); err != nil {
		return err
	}
	total, err := memTotal()
	if err != nil {
		return err
	}
	if total <= 2*memoryHeadroom {
		return fmt.Errorf("the VM has %d bytes, too few to bound the sandbox under %d bytes of headroom; the provider refuses below 128 MiB", total, memoryHeadroom)
	}
	if err := cgroup.SetMemoryMax(dir, total-memoryHeadroom); err != nil {
		return err
	}
	if err := cgroup.SetMemorySwapMax(dir, 0); err != nil {
		return err
	}
	if err := cgroup.SetOOMGroup(dir); err != nil {
		return err
	}
	if err := cgroup.Add(dir, os.Getpid()); err != nil {
		return fmt.Errorf("move PID 1 into the sandbox cgroup: %w", err)
	}

	return nil
}

// remountCgroup moves PID 1 beside the sandbox cgroup, mounts cgroup2 again inside the new namespace, and opens the sandbox cgroup every guest process is born into.
func remountCgroup() (*os.File, error) {
	// Only the host view reaches the sibling; the namespace's own mount shows the sandbox cgroup as its root.
	if err := cgroup.Add(filepath.Join(cgroupRoot, initCgroup), os.Getpid()); err != nil {
		return nil, fmt.Errorf("move PID 1 out of the sandbox cgroup: %w", err)
	}
	if err := unix.Unmount(cgroupRoot, unix.MNT_DETACH); err != nil {
		return nil, fmt.Errorf("unmount the host view of %s: %w", cgroupRoot, err)
	}
	if err := mountOnce("cgroup2", cgroupRoot, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC); err != nil {
		return nil, err
	}
	bound, err := os.Open(cgroupRoot)
	if err != nil {
		return nil, fmt.Errorf("open the sandbox cgroup: %w", err)
	}

	return bound, nil
}

// freezeWait is how long a pause waits for every guest process to stop; one asleep in the kernel holds the freeze until it wakes.
const freezeWait = 10 * time.Second

// freezeBound stops every process in the sandbox cgroup, and returns once the kernel says each one has.
func freezeBound(bound *os.File) error {
	if bound == nil {
		return nil
	}
	if err := writeAt(bound, "cgroup.freeze", "1"); err != nil {
		return fmt.Errorf("freeze the sandbox cgroup: %w", err)
	}
	deadline := time.Now().Add(freezeWait)
	for {
		events, err := readAt(bound, "cgroup.events")
		if err != nil {
			return errors.Join(fmt.Errorf("read the sandbox cgroup events: %w", err), thawBound(bound))
		}
		if slices.Contains(strings.Split(events, "\n"), "frozen 1") {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.Join(fmt.Errorf("the sandbox cgroup did not freeze within %s", freezeWait), thawBound(bound))
		}
		time.Sleep(time.Millisecond)
	}
}

// thawBound lets the sandbox cgroup run again; one that is not frozen takes it as a no-op.
func thawBound(bound *os.File) error {
	if bound == nil {
		return nil
	}
	if err := writeAt(bound, "cgroup.freeze", "0"); err != nil {
		return fmt.Errorf("thaw the sandbox cgroup: %w", err)
	}

	return nil
}

// writeAt and readAt go through the open cgroup, which a guest process that mounts over /sys/fs/cgroup cannot move.
func writeAt(dir *os.File, name, value string) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	_, err = f.WriteString(value)

	return errors.Join(err, f.Close())
}

func readAt(dir *os.File, name string) (string, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	b, err := io.ReadAll(f)

	return string(b), errors.Join(err, f.Close())
}

// memTotal is the VM's memory as the kernel reports it, which is what a bound must stay under.
func memTotal() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("read MemTotal: %w", err)
		}

		return kib << 10, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}

	return 0, fmt.Errorf("/proc/meminfo has no MemTotal")
}

// oomKilledGuest says the sandbox cgroup's own bound was hit, which under memory.oom.group took every guest process.
func oomKilledGuest() (bool, error) {
	events, err := cgroup.LocalMemoryEvents(cgroupRoot)
	if err != nil {
		return false, err
	}

	return events.OOM > 0, nil
}
