//go:build integration

// Package hostclean refuses a box an earlier integration run left dirty, and gives one back clean.
package hostclean

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	held, err := leftHeld(prefixes)
	if err != nil {
		return nil, err
	}
	roots, err := leftRoots(prefixes)
	if err != nil {
		return nil, err
	}

	// A root goes last: what it holds is named only by what lives under it.
	return append(held, roots...), nil
}

// Sweep takes back everything Find names, then the host network once nothing holds it, and what it could not take is what the error names.
func Sweep(prefixes ...string) error {
	left, err := Find(prefixes...)
	if err != nil {
		return err
	}

	return errors.Join(removeEach(left), sweepHostNet())
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

func removeAll(path string) func() error {
	return func() error { return os.RemoveAll(path) }
}
