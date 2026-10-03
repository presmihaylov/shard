//go:build integration && darwin

package hostclean

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// shimGrace bounds the wait for a killed shim to leave the process table.
const shimGrace = 5 * time.Second

// leftHeld names the vm shims: on a Mac they are all a run holds outside its roots.
func leftHeld(prefixes, _ []string) ([]Leftover, error) {
	return leftShims(prefixes)
}

// heldBy has nothing to name: a Mac run hands its disks to the VM and mounts nothing on the host.
func heldBy(string) ([]Leftover, error) { return nil, nil }

// sweepShared has nothing to take: the guest network of a Mac run lives inside the test process, and a Mac has no cgroup.
func sweepShared() error { return nil }

// noteParent has nothing to note: a Mac has no cgroup parent.
func noteParent() error { return nil }

// leftShims names every shim whose config puts its socket under a root of ours, which is the only mark a shim carries.
func leftShims(prefixes []string) ([]Leftover, error) {
	listed, err := exec.Command("ps", "-axww", "-o", "pid=", "-o", "args=").Output()
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}

	var out []Leftover
	for line := range strings.SplitSeq(string(listed), "\n") {
		field, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !socketUnder(args, prefixes) {
			continue
		}
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("read the pid of %q: %w", line, err)
		}
		out = append(out, Leftover{What: "the vm shim", Path: field, remove: killShim(pid)})
	}

	return out, nil
}

func socketUnder(args string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.Contains(args, `"socket":"`+prefix) {
			return true
		}
	}

	return false
}

// killShim waits for the pid to go, because the root it lives under is removed next.
func killShim(pid int) func() error {
	return func() error {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill %d: %w", pid, err)
		}
		for deadline := time.Now().Add(shimGrace); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				return nil
			}
		}

		return fmt.Errorf("pid %d still runs %s after a SIGKILL", pid, shimGrace)
	}
}
