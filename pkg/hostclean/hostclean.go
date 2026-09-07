//go:build integration

// Package hostclean refuses a box an earlier integration run left dirty, and gives one back clean.
package hostclean

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/presmihaylov/shard/pkg/netns"
)

// linkPattern is how services/network names a host veth: the tests must not know the sandbox ids.
var linkPattern = regexp.MustCompile(`shardv[0-9]+`)

// mountinfo is where the kernel lists what is mounted, and the only account of a mount a run leaked.
const mountinfo = "/proc/self/mountinfo"

// Leftover is one thing a run left on the host, with the way to take it back.
type Leftover struct {
	What string
	Path string

	remove func() error
}

func (l Leftover) String() string { return l.What + " " + l.Path }

// Find lists what an integration run leaves when it does not tear down. The prefixes are the temp
// roots of the package that asks, so one package never reports the roots of another.
func Find(prefixes ...string) ([]Leftover, error) {
	mounts, err := leftMounts(prefixes)
	if err != nil {
		return nil, err
	}
	namespaces, err := leftNamespaces()
	if err != nil {
		return nil, err
	}
	links, err := leftLinks()
	if err != nil {
		return nil, err
	}
	roots, err := leftRoots(prefixes)
	if err != nil {
		return nil, err
	}

	// The order is the order Sweep must take them in: a mount pins the root it lives under.
	return append(append(append(mounts, namespaces...), links...), roots...), nil
}

// Sweep takes back everything Find names, and what it could not take is what the error names.
//
// It is safe only because Refuse ran first: a box that carried no sandbox at the start carries only
// this run's own at the end, so a wholesale sweep of the namespaces and the links takes nothing else.
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

// Refuse fails a run on a box that already carries a sandbox. The lease pool lives under this run's
// own root, so it would hand out an address another root holds and delete that sandbox's veth.
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

// leftNamespaces names every network namespace on the host, because a sandbox is the only thing here
// that makes one and a box that runs these tests runs nothing else.
func leftNamespaces() ([]Leftover, error) {
	entries, err := os.ReadDir(netns.RunDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", netns.RunDir, err)
	}

	out := make([]Leftover, 0, len(entries))
	for _, entry := range entries {
		out = append(out, Leftover{What: "the namespace", Path: entry.Name(), remove: run("ip", "netns", "delete", entry.Name())})
	}

	return out, nil
}

func leftLinks() ([]Leftover, error) {
	listed, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("list the host links: %w", err)
	}

	var out []Leftover
	for _, name := range unique(linkPattern.FindAllString(string(listed), -1)) {
		out = append(out, Leftover{What: "the sandbox link", Path: name, remove: run("ip", "link", "delete", name)})
	}

	return out, nil
}

func leftRoots(prefixes []string) ([]Leftover, error) {
	var out []Leftover
	for _, prefix := range prefixes {
		matches, err := filepath.Glob(prefix + "*")
		if err != nil {
			return nil, fmt.Errorf("list the roots under %s: %w", prefix, err)
		}
		for _, match := range matches {
			out = append(out, Leftover{What: "the temp root", Path: match, remove: removeAll(match)})
		}
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

func unique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)

	return out
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
