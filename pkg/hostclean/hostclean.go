//go:build integration

// Package hostclean refuses a box an earlier integration run left dirty, and gives one back clean.
package hostclean

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/presmihaylov/shard/pkg/netns"
)

// sandboxDir and recordFile are where the daemon keeps a sandbox record, under a root of its own.
const (
	sandboxDir = "sandboxes"
	recordFile = "sandbox.json"
)

// mountinfo is where the kernel lists what is mounted, and the only account of a mount a run leaked.
const mountinfo = "/proc/self/mountinfo"

// Leftover is one thing a run left on the host, with the way to take it back.
type Leftover struct {
	What string
	Path string

	remove func() error
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

	// The order is the order Sweep must take them in: a mount pins the root it lives under, and the
	// record under that root is the only handle by which the namespace and the link can be found.
	return append(append(mounts, sandboxes...), roots...), nil
}

// Sweep takes back everything Find names, and what it could not take is what the error names.
func Sweep(prefixes ...string) error {
	left, err := Find(prefixes...)
	if err != nil {
		return err
	}

	return removeEach(left)
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

// Unmount takes back only the mounts under the roots it is given, and touches no other host state.
// A run uses it to give one root back while the rest of the suite still holds sandboxes of its own.
func Unmount(prefixes ...string) error {
	mounts, err := leftMounts(prefixes)
	if err != nil {
		return err
	}

	return removeEach(mounts)
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

	return fmt.Errorf("the host carries what an earlier run left, and this run must not remove it:\n\t%s", strings.Join(names, "\n\t"))
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

// sandboxOf names what one record still holds on the host. A namespace or a link that is already
// gone is named by no leftover, so a teardown that ran twice reports nothing the second time.
func sandboxOf(root, id string) []Leftover {
	var out []Leftover
	if _, err := os.Stat(netns.NamespacePath(id)); err == nil {
		out = append(out, Leftover{What: "the namespace", Path: id, remove: run("ip", "netns", "delete", id)})
	}

	link := hostInterface(filepath.Join(root, sandboxDir, id, recordFile))
	if link == "" {
		return out
	}
	if exec.Command("ip", "link", "show", link).Run() != nil {
		return out
	}

	return append(out, Leftover{What: "the sandbox link", Path: link, remove: run("ip", "link", "delete", link)})
}

// hostInterface reads the one field of a record this package needs, and answers "" for a record a
// crashed run never finished writing.
func hostInterface(record string) string {
	blob, err := os.ReadFile(record)
	if err != nil {
		return ""
	}

	var held struct {
		HostInterface string `json:"host_interface"`
	}
	if json.Unmarshal(blob, &held) != nil {
		return ""
	}

	return held.HostInterface
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
