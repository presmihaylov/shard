//go:build integration

// Package hostclean refuses a box an earlier integration run left dirty, and gives one back clean.
package hostclean

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/pkg/xfs"
)

// sandboxDir and recordFile are where the daemon keeps a sandbox record, under a root of its own.
const (
	sandboxDir = "sandboxes"
	recordFile = "sandbox.json"
)

// cgroupParent is the one cgroup the daemon puts every sandbox under, by id.
const cgroupParent = "shard"

// apiSockFlag is how pkg/firecracker hands a vmm its api socket, and a socket under a root of ours makes the vmm ours.
const apiSockFlag = "--api-sock"

// killGrace bounds the wait for a killed vmm to let go of its cgroup, and pollInterval paces that wait.
const (
	killGrace    = 10 * time.Second
	pollInterval = 100 * time.Millisecond
)

// runtimes maps the provider a record names to the binary whose state the daemon keeps under the root, by that name.
var runtimes = map[string]string{"gvisor": "runsc", "sysbox": "sysbox-runc", "runc": "runc"}

// mountinfo is where the kernel lists what is mounted, and the only account of a mount a run leaked.
const mountinfo = "/proc/self/mountinfo"

// The bridge and the policy tables every root on the host shares, by the names services/network gives them.
const (
	hostBridge = "shard0"
	hostTable  = "shard"
)

var hostTableFamilies = []string{"inet", "bridge"}

// Leftover is one thing a run left on the host, with the way to take it back.
type Leftover struct {
	What string
	Path string

	remove func() error
	// pin holds a vmm by its pidfd from Find to the kill, so its pid never passes to another process in between.
	pin *os.File
}

func (l Leftover) String() string { return l.What + " " + l.Path }

// Find lists what an integration run leaves when it does not tear down. Everything it names is owned
// by a root of the package that asks, so a run beside the systemd unit reports and takes none of its.
func Find(prefixes ...string) ([]Leftover, error) {
	mounts, err := leftMounts(prefixes)
	if err != nil {
		return nil, err
	}
	sandboxes, err := leftSandboxes(prefixes)
	if err != nil {
		return nil, err
	}
	roots, err := leftRoots(prefixes)
	if err != nil {
		return nil, err
	}
	lines, err := leftFstab(prefixes)
	if err != nil {
		return nil, err
	}
	// The pins come last, so no failure after them leaves one open.
	vmms, err := leftVMMs(prefixes)
	if err != nil {
		return nil, err
	}

	// The order is the order Sweep must take them in: a vmm holds its cgroup and its tap, a mount pins the root
	// it lives under, and the record under that root is the only handle by which the namespace and the link can be found.
	return slices.Concat(vmms, mounts, sandboxes, roots, lines), nil
}

// Sweep takes back everything Find names, then what every root shares once nothing holds it, and what it could not take is what the error names.
func Sweep(prefixes ...string) error {
	left, err := Find(prefixes...)
	if err != nil {
		return err
	}

	return errors.Join(removeEach(left), sweepShared())
}

// sweepShared drops the bridge, the tables and the cgroup parent the daemon never drops (SHARD-272), unless a run on another root still holds them.
func sweepShared() error {
	held, err := hostNetHeld()
	if err != nil {
		return err
	}
	if held {
		return nil
	}

	listed, err := exec.Command("nft", "list", "tables").Output()
	if err != nil {
		return fmt.Errorf("list the nft tables: %w", err)
	}
	tables := strings.Split(string(listed), "\n")

	var left []Leftover
	for _, family := range hostTableFamilies {
		if slices.Contains(tables, "table "+family+" "+hostTable) {
			left = append(left, Leftover{What: "the " + family + " table", Path: hostTable, remove: run("nft", "delete", "table", family, hostTable)})
		}
	}
	if shown(hostBridge) {
		left = append(left, Leftover{What: "the bridge", Path: hostBridge, remove: deleteLink(hostBridge)})
	}
	parent := filepath.Join(cgroup.Root, cgroupParent)
	idle, err := idleCgroup(parent)
	if err != nil {
		return err
	}
	if idle {
		left = append(left, Leftover{What: "the cgroup parent", Path: parent, remove: func() error { return cgroup.Remove(parent) }})
	}

	return removeEach(left)
}

// idleCgroup is whether dir exists and holds no sandbox; every provider makes the parent again on its next create.
func idleCgroup(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list the cgroups under %s: %w", dir, err)
	}

	return !slices.ContainsFunc(entries, os.DirEntry.IsDir), nil
}

// hostNetHeld is whether a sandbox of any root still has a port on the bridge, or a daemon still serves the proxy.
func hostNetHeld() (bool, error) {
	ports, err := os.ReadDir(filepath.Join("/sys/class/net", hostBridge, "brif"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("list the ports of the bridge %s: %w", hostBridge, err)
	}
	if len(ports) > 0 {
		return true, nil
	}

	filter := fmt.Sprintf("( sport = :%d or sport = :%d )", proxy.PlainPort, proxy.TLSPort)
	listeners, err := exec.Command("ss", "-Hltn", filter).Output()
	if err != nil {
		return false, fmt.Errorf("list the listeners on the proxy ports: %w", err)
	}

	return strings.TrimSpace(string(listeners)) != "", nil
}

// removeEach stops at nothing: a step that fails still leaves the steps below it to run.
func removeEach(left []Leftover) error {
	var failed []string
	for _, l := range left {
		if err := l.remove(); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", l, err))
		}
	}
	if len(failed) == 0 {
		return nil
	}

	return fmt.Errorf("the host still carries what this run made:\n\t%s", strings.Join(failed, "\n\t"))
}

// Release gives back what one root's sandboxes and mounts still hold and touches no other root, so a run can remove it while the suite runs on.
func Release(root string) error {
	sandboxes, err := sandboxesOf(root)
	if err != nil {
		return err
	}
	mounts, err := leftMounts([]string{root})
	if err != nil {
		return err
	}
	vmms, err := leftVMMs([]string{filepath.Join(root, sandboxDir) + string(filepath.Separator)})
	if err != nil {
		return err
	}

	return removeEach(slices.Concat(vmms, sandboxes, mounts))
}

// Refuse fails a run on a root an earlier run of the same package left, because its lease pool lives
// in that root: a new pool would hand out an address the old one holds and delete that sandbox's veth.
func Refuse(prefixes ...string) error {
	left, err := Find(prefixes...)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		return nil
	}

	names := make([]string, 0, len(left))
	for _, l := range left {
		names = append(names, l.String())
	}

	return errors.Join(fmt.Errorf("the host carries what an earlier run left, and this run must not remove it:\n\t%s", strings.Join(names, "\n\t")), unpin(left))
}

// unpin lets go of the vmms a Find pinned and nothing removed.
func unpin(left []Leftover) error {
	var errs []error
	for _, l := range left {
		if l.pin == nil {
			continue
		}
		if err := l.pin.Close(); err != nil {
			errs = append(errs, fmt.Errorf("unpin %s: %w", l, err))
		}
	}

	return errors.Join(errs...)
}

// leftMounts names the mounts under a temp root, deepest first: an overlay pins the root beneath it.
func leftMounts(prefixes []string) ([]Leftover, error) {
	listed, err := os.ReadFile(mountinfo)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mountinfo, err)
	}

	points := mountPoints(string(listed), prefixes)

	sort.Sort(sort.Reverse(sort.StringSlice(points)))

	out := make([]Leftover, 0, len(points))
	for _, point := range points {
		out = append(out, Leftover{What: "the mount", Path: point, remove: run("umount", "-l", point)})
	}

	return out, nil
}

// leftSandboxes names the namespace and the veth of every sandbox a leftover root still records. The
// record is what makes them ours: a namespace no root of this package names belongs to another run.
func leftSandboxes(prefixes []string) ([]Leftover, error) {
	roots, err := match(prefixes)
	if err != nil {
		return nil, err
	}

	var out []Leftover
	for _, root := range roots {
		held, err := sandboxesOf(root)
		if err != nil {
			return nil, err
		}
		out = append(out, held...)
	}

	return out, nil
}

// sandboxesOf names what every record under one root still holds.
func sandboxesOf(root string) ([]Leftover, error) {
	entries, err := os.ReadDir(filepath.Join(root, sandboxDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the sandboxes of %s: %w", root, err)
	}

	var out []Leftover
	for _, entry := range entries {
		out = append(out, sandboxOf(root, entry.Name())...)
	}

	return out, nil
}

// sandboxOf names what one record still holds on the host; anything already gone is named by no leftover.
func sandboxOf(root, id string) []Leftover {
	var out []Leftover
	rec := readRecord(filepath.Join(root, sandboxDir, id, recordFile))

	// runsc keeps its state flat under the root, so only the record can name a sandbox; a forced delete of one the runtime no longer holds is a no-op on every runtime.
	if binary, ok := runtimes[rec.Provider]; ok {
		state := filepath.Join(root, binary)
		out = append(out, Leftover{What: "the sandbox", Path: id, remove: run(binary, "--root", state, "delete", "--force", id)})
	}
	// A stop keeps the cgroup for the rm that never came.
	if group := filepath.Join(cgroup.Root, cgroupParent, id); exists(group) {
		out = append(out, Leftover{What: "the cgroup", Path: group, remove: removeCgroup(group)})
	}
	if exists(netns.NamespacePath(id)) {
		out = append(out, Leftover{What: "the namespace", Path: id, remove: run("ip", "netns", "delete", id)})
	}

	if rec.HostInterface == "" || !shown(rec.HostInterface) {
		return out
	}

	return append(out, Leftover{What: "the sandbox link", Path: rec.HostInterface, remove: deleteLink(rec.HostInterface)})
}

// shown is whether the host still lists the link, which is what makes it ours to take.
func shown(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

// deleteLink takes a link that is gone as swept: the namespace delete above takes the veth pair with it (SHARD-227).
func deleteLink(name string) func() error {
	return func() error {
		if err := run("ip", "link", "delete", name)(); err != nil && shown(name) {
			return err
		}

		return nil
	}
}

// record is the two fields of a sandbox record this package reads: who holds the sandbox, and its veth.
type record struct {
	Provider      string `json:"provider"`
	HostInterface string `json:"host_interface"`
}

// readRecord answers an empty record for one a crashed run never finished writing.
func readRecord(path string) record {
	blob, err := os.ReadFile(path)
	if err != nil {
		return record{}
	}

	var held record
	if json.Unmarshal(blob, &held) != nil {
		return record{}
	}

	return held
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// leftVMMs names every vmm whose api socket sits under a root of ours, so one is found after the record that started it is gone.
func leftVMMs(prefixes []string) ([]Leftover, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}

	var out []Leftover
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("read the command line of %d: %w", pid, err), unpin(out))
		}
		sock := apiSocket(strings.Split(string(cmdline), "\x00"))
		if sock == "" || !hasPrefix(sock, prefixes) {
			continue
		}
		vmm, ours, err := pinVMM(pid, sock)
		if err != nil {
			return nil, errors.Join(err, unpin(out))
		}
		if ours {
			out = append(out, vmm)
		}
	}

	return out, nil
}

// apiSocket is the whole argument after --api-sock, or "" for a process that names none.
func apiSocket(argv []string) string {
	i := slices.Index(argv, apiSockFlag)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}

	return argv[i+1]
}

// gone is whether a read under /proc failed only because the process exited.
func gone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// removeCgroup waits out the moment a killed vmm still holds its cgroup after it exits.
func removeCgroup(dir string) func() error {
	return func() error {
		deadline := time.Now().Add(killGrace)
		for {
			err := cgroup.Remove(dir)
			if !errors.Is(err, syscall.EBUSY) || !time.Now().Before(deadline) {
				return err
			}
			time.Sleep(pollInterval)
		}
	}
}

func leftRoots(prefixes []string) ([]Leftover, error) {
	roots, err := match(prefixes)
	if err != nil {
		return nil, err
	}

	out := make([]Leftover, 0, len(roots))
	for _, root := range roots {
		out = append(out, Leftover{What: "the temp root", Path: root, remove: removeAll(root)})
	}

	return out, nil
}

// leftFstab names the line that mounts a data image at a root of ours on boot, which outlives the image and the root.
func leftFstab(prefixes []string) ([]Leftover, error) {
	loops, err := xfs.FstabLoops()
	if err != nil {
		return nil, err
	}

	var out []Leftover
	for _, loop := range loops {
		if hasPrefix(loop.Image, prefixes) && hasPrefix(loop.Point, prefixes) {
			out = append(out, Leftover{What: "the fstab line", Path: loop.Point, remove: func() error { return xfs.RemoveFstab(loop.Image, loop.Point) }})
		}
	}

	return out, nil
}

// match answers the roots an earlier run of this package left, which is the whole of what it owns.
func match(prefixes []string) ([]string, error) {
	var out []string
	for _, prefix := range prefixes {
		matches, err := filepath.Glob(prefix + "*")
		if err != nil {
			return nil, fmt.Errorf("list the roots under %s: %w", prefix, err)
		}
		out = append(out, matches...)
	}

	return out, nil
}

// mountPoints reads the fifth field of each line of mountinfo, which is where the mount is attached.
func mountPoints(listed string, prefixes []string) []string {
	var points []string
	for line := range strings.SplitSeq(listed, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if hasPrefix(fields[4], prefixes) {
			points = append(points, fields[4])
		}
	}

	return points
}

func hasPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

func run(binary string, args ...string) func() error {
	return func() error {
		out, err := exec.Command(binary, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(string(out)))
		}

		return nil
	}
}

func removeAll(path string) func() error {
	return func() error { return os.RemoveAll(path) }
}
