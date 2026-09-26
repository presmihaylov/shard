package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/cgroup"
)

// cgroupRoot is the cgroup2 mount; after the re-exec it shows the sandbox cgroup as the root of the guest's own namespace.
const cgroupRoot = "/sys/fs/cgroup"

// memoryHeadroom is what the bound leaves the kernel and shard-init, so the cgroup's OOM killer runs before the VM's own.
const memoryHeadroom int64 = 32 << 20

// boundMemory makes the sandbox cgroup and moves PID 1 into it, so every guest process is born under the bound.
// PID 1 itself is exempt from the OOM killer, so a group kill takes the guest's processes and leaves the supervisor to report it.
func boundMemory() error {
	if err := cgroup.Delegate(cgroupRoot, "memory"); err != nil {
		return fmt.Errorf("enable the memory controller: %w", err)
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
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0o600); err != nil {
		return fmt.Errorf("exempt PID 1 from the OOM killer: %w", err)
	}

	return nil
}

// remountCgroup mounts cgroup2 again inside the new cgroup namespace, so the guest sees the sandbox cgroup as its root.
func remountCgroup() error {
	if err := unix.Unmount(cgroupRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount the host view of %s: %w", cgroupRoot, err)
	}

	return mountOnce("cgroup2", cgroupRoot, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC)
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

// exposeFlag is the child's own first argument: a re-exec of shard-init that runs before the workload.
const exposeFlag = "-expose"

// expose gives up the OOM exemption inherited from PID 1 and execs the workload in its place; it returns only on a failure.
func expose(binary string, argv []string) error {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("0"), 0o600); err != nil {
		return fmt.Errorf("expose %q to the OOM killer: %w", argv[0], err)
	}

	return fmt.Errorf("exec %q: %w", argv[0], syscall.Exec(binary, argv, os.Environ()))
}
