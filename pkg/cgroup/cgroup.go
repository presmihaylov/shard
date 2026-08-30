// Package cgroup drives cgroup v2 control files. It knows nothing about sandboxes: which cgroup a
// process lands in, and what its bounds are, is the caller's policy and never this driver's.
package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Root is where a Linux host mounts the cgroup v2 hierarchy.
const Root = "/sys/fs/cgroup"

// ErrNotFound is what a write to a cgroup that is not there returns. Match it with errors.Is.
var ErrNotFound = errors.New("no such cgroup")

// ErrNoController is what a cgroup that exists without the control file returns. A cgroup v1 host,
// and a v2 host whose parent does not delegate the memory controller, both land here.
var ErrNoController = errors.New("the cgroup has no such controller: the host may be cgroup v1, or the controller is not delegated")

// SetMemoryHigh sets the throttle the kernel reclaims against. It is not a ceiling: a cgroup over it
// is put under reclaim pressure and slowed down, never killed. memory.max is the ceiling.
func SetMemoryHigh(dir string, bytes int64) error {
	return write(dir, "memory.high", strconv.FormatInt(bytes, 10))
}

// MemoryHigh reads the throttle back. It answers -1 for the literal "max", which is no throttle.
func MemoryHigh(dir string) (int64, error) {
	return readBound(dir, "memory.high")
}

// SetMemoryMax sets the ceiling the host OOM killer answers. A cgroup over it loses a process.
func SetMemoryMax(dir string, bytes int64) error {
	return write(dir, "memory.max", strconv.FormatInt(bytes, 10))
}

// MemoryMax reads the ceiling the OOM killer answers. It answers -1 for the literal "max", which is
// no ceiling at all.
func MemoryMax(dir string) (int64, error) {
	return readBound(dir, "memory.max")
}

// SetMemorySwapMax bounds what the cgroup may push to swap. Zero pins it to none, which is what
// makes memory.high a wall: a cgroup that can swap reclaims under the throttle and never stops there.
func SetMemorySwapMax(dir string, bytes int64) error {
	return write(dir, "memory.swap.max", strconv.FormatInt(bytes, 10))
}

// SetOOMGroup makes the OOM killer take every process in the cgroup, not the one it would pick.
func SetOOMGroup(dir string) error {
	return write(dir, "memory.oom.group", "1")
}

// Events counts what the kernel did to a cgroup. It survives the death of every process in one, so
// it is the only record of why a sandbox is gone once its own processes cannot be asked.
type Events struct {
	// OOM counts the times this cgroup hit its own ceiling and called the OOM killer.
	OOM int64
	// OOMKill counts processes killed here by any OOM killer, this cgroup's or the host's.
	OOMKill int64
}

// MemoryEvents reads memory.events. A cgroup that is gone answers ErrNotFound, which is the ordinary
// answer for a sandbox that stopped cleanly, because runsc removes its cgroup on delete.
func MemoryEvents(dir string) (Events, error) {
	raw, err := read(dir, "memory.events")
	if err != nil {
		return Events{}, err
	}

	var events Events
	for line := range strings.SplitSeq(raw, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}

		count, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Events{}, fmt.Errorf("read %s: %q counts %q, which is not a number", filepath.Join(dir, "memory.events"), key, value)
		}

		switch key {
		case "oom":
			events.OOM = count
		case "oom_kill":
			events.OOMKill = count
		}
	}

	return events, nil
}

// Ensure makes a cgroup exist, and one that already does is fine.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("make the cgroup %s: %w", dir, err)
	}

	return nil
}

// Remove drops an empty cgroup, and one that is already gone counts as dropped.
func Remove(dir string) error {
	err := os.Remove(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove the cgroup %s: %w", dir, err)
	}

	return nil
}

func write(dir, file, value string) error {
	path := filepath.Join(dir, file)

	err := os.WriteFile(path, []byte(value), 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("write %s: %w", path, missing(dir))
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// readBound reads one memory control file, where the literal "max" means the bound is not set.
func readBound(dir, file string) (int64, error) {
	raw, err := read(dir, file)
	if err != nil {
		return 0, err
	}
	if raw == "max" {
		return -1, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Join(dir, file), err)
	}

	return value, nil
}

func read(dir, file string) (string, error) {
	path := filepath.Join(dir, file)

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", path, missing(dir))
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	return strings.TrimSpace(string(raw)), nil
}

// missing tells the two ways a control file goes missing apart, because they need opposite answers:
// a cgroup that is gone is the caller's problem, and a controller that is gone is the host's.
func missing(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}

	return ErrNoController
}
