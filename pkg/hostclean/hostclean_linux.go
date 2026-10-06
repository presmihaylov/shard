//go:build integration && linux

package hostclean

import (
	"context"
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
	"github.com/presmihaylov/shard/pkg/hostfw"
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

// parentPath is where the host keeps that cgroup; a test points it at a dir of its own.
var parentPath = filepath.Join(cgroup.Root, cgroupParent)

// parentMade is whether Refuse found no cgroup parent, which makes the parent this run's to drop.
var parentMade bool

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

// The bridge, the policy tables and the host firewall hole every root on the host shares, by the names services/network gives them.
const (
	hostBridge = "shard0"
	hostTable  = "shard"
	hostZone   = "shard"
	hostPolicy = "shard-forwarding"
	hostMarker = "managed-by-shard"
)

var hostTableFamilies = []string{"inet", "bridge"}

// leftHeld names what the roots hold in the order Sweep takes it: a vmm holds its cgroup and tap, a mount pins its root and loop, a record names the netns and link, and a line outlives its image.
func leftHeld(prefixes, roots []string) ([]Leftover, error) {
	mounts, err := leftMounts(prefixes)
	if err != nil {
		return nil, err
	}
	loops, err := leftLoops(prefixes)
	if err != nil {
		return nil, err
	}
	sandboxes, err := leftSandboxes(roots)
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

	return slices.Concat(vmms, mounts, loops, sandboxes, lines), nil
}

// heldBy names what one root's vmms, sandboxes and mounts still hold, and nothing of another root.
func heldBy(root string) ([]Leftover, error) {
	sandboxes, err := sandboxesOf(root)
	if err != nil {
		return nil, err
	}
	mounts, err := leftMounts([]string{root})
	if err != nil {
		return nil, err
	}
	vmms, err := leftVMMs([]string{filepath.Join(root, sandboxDir) + string(filepath.Separator)})
	if err != nil {
		return nil, err
	}

	return slices.Concat(vmms, sandboxes, mounts), nil
}

// noteParent records whether the cgroup parent was missing before the run, which makes it this run's to drop.
func noteParent() error {
	_, err := os.Stat(parentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat the cgroup parent %s: %w", parentPath, err)
	}
	parentMade = errors.Is(err, os.ErrNotExist)

	return nil
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
	hole := hostfw.Hole{Name: hostZone, Marker: hostMarker, Interface: hostBridge, Policy: hostPolicy}
	left = append(left, Leftover{What: "the host firewall accepts", Path: hostZone, remove: func() error { return hostfw.Local().Close(context.Background(), hole) }})
	if shown(hostBridge) {
		left = append(left, Leftover{What: "the bridge", Path: hostBridge, remove: deleteLink(hostBridge)})
	}
	parent, err := leftParent()
	if err != nil {
		return err
	}

	return removeEach(append(left, parent...))
}

// leftParent names the cgroup parent only when this run made it and no sandbox of any root is under it, so a parent the host had stays as it was.
func leftParent() ([]Leftover, error) {
	if !parentMade {
		return nil, nil
	}
	idle, err := idleCgroup(parentPath)
	if err != nil || !idle {
		return nil, err
	}

	return []Leftover{{What: "the cgroup parent", Path: parentPath, remove: func() error { return cgroup.Remove(parentPath) }}}, nil
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
func leftSandboxes(roots []string) ([]Leftover, error) {
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
	if group := filepath.Join(parentPath, id); exists(group) {
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

// loopDevices is where the kernel names the file behind each bound loop device.
const loopDevices = "/sys/block/loop*/loop/backing_file"

// leftLoops names every loop device over an image of ours, which a mount that went away without its loop leaves bound.
func leftLoops(prefixes []string) ([]Leftover, error) {
	bound, err := filepath.Glob(loopDevices)
	if err != nil {
		return nil, fmt.Errorf("list the loop devices: %w", err)
	}

	var out []Leftover
	for _, file := range bound {
		dev := "/dev/" + filepath.Base(filepath.Dir(filepath.Dir(file)))
		image, err := loopImage(dev)
		if err != nil {
			return nil, err
		}
		if image != "" && hasPrefix(image, prefixes) {
			out = append(out, Leftover{What: "the loop", Path: dev, remove: detachLoop(dev, image)})
		}
	}

	return out, nil
}

// loopImage is the file a loop device reads, or "" for one bound to nothing.
func loopImage(dev string) (string, error) {
	file := filepath.Join("/sys/block", filepath.Base(dev), "loop", "backing_file")
	blob, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}

	return strings.TrimSuffix(strings.TrimSpace(string(blob)), " (deleted)"), nil
}

// detachLoop waits out the moment an unmount above still holds the loop, and takes one already cleared as detached.
func detachLoop(dev, image string) func() error {
	return func() error {
		deadline := time.Now().Add(killGrace)
		for {
			bound, err := loopImage(dev)
			if err != nil || bound != image {
				return err
			}
			// A detach that loses the race to the clear is moot, so only a loop still bound at the deadline names its error.
			detach := run("losetup", "-d", dev)()
			if !time.Now().Before(deadline) {
				return errors.Join(fmt.Errorf("%s still reads %s", dev, image), detach)
			}
			time.Sleep(pollInterval)
		}
	}
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
