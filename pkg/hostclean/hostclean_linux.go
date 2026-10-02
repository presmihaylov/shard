//go:build integration && linux

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
	"strings"

	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/proxy"
)

// sandboxDir and recordFile are where the daemon keeps a sandbox record, under a root of its own.
const (
	sandboxDir = "sandboxes"
	recordFile = "sandbox.json"
)

// cgroupParent is the one cgroup the daemon puts every sandbox under, by id.
const cgroupParent = "shard"

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

// leftHeld names the mounts before the sandboxes: a mount pins its root, and the record under that root is the only handle on the namespace and the link.
func leftHeld(prefixes []string) ([]Leftover, error) {
	mounts, err := leftMounts(prefixes)
	if err != nil {
		return nil, err
	}
	sandboxes, err := leftSandboxes(prefixes)
	if err != nil {
		return nil, err
	}

	return append(mounts, sandboxes...), nil
}

// sweepHostNet drops the bridge and the tables the daemon never drops (SHARD-272), unless a run on another root still holds them.
func sweepHostNet() error {
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

	return removeEach(left)
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
func leftSandboxes(prefixes []string) ([]Leftover, error) {
	roots, err := match(prefixes)
	if err != nil {
		return nil, err
	}

	var out []Leftover
	for _, root := range roots {
		entries, err := os.ReadDir(filepath.Join(root, sandboxDir))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the sandboxes of %s: %w", root, err)
		}

		for _, entry := range entries {
			out = append(out, sandboxOf(root, entry.Name())...)
		}
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
		out = append(out, Leftover{What: "the cgroup", Path: group, remove: func() error { return cgroup.Remove(group) }})
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
