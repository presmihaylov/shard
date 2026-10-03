//go:build integration

// Package hostclean refuses a box an earlier integration run left dirty, and gives one back clean.
package hostclean

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

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
	roots, files, err := match(prefixes)
	if err != nil {
		return nil, err
	}
	held, err := leftHeld(prefixes, roots)
	if err != nil {
		return nil, err
	}

	// A root goes after what it holds, which is named only by what lives under it, and an image beside it after its loop and line.
	return slices.Concat(held, taken("the temp root", roots), taken("the temp file", files)), nil
}

// Sweep takes back everything Find names, then what every root shares once nothing holds it, and what it could not take is what the error names.
func Sweep(prefixes ...string) error {
	left, err := Find(prefixes...)
	if err != nil {
		return err
	}

	return errors.Join(removeEach(left), sweepShared())
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
	held, err := heldBy(root)
	if err != nil {
		return err
	}

	return removeEach(held)
}

// Refuse fails a run on a root an earlier run of the same package left, because its lease pool lives
// in that root: a new pool would hand out an address the old one holds and delete that sandbox's veth.
func Refuse(prefixes ...string) error {
	if err := noteParent(); err != nil {
		return err
	}
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

func taken(what string, paths []string) []Leftover {
	out := make([]Leftover, 0, len(paths))
	for _, path := range paths {
		out = append(out, Leftover{What: what, Path: path, remove: removeAll(path)})
	}

	return out
}

// match answers what an earlier run of this package left, which is the whole of what it owns: the roots, and the files beside them such as a data image and its lock.
func match(prefixes []string) (roots, files []string, err error) {
	for _, prefix := range prefixes {
		matches, err := filepath.Glob(prefix + "*")
		if err != nil {
			return nil, nil, fmt.Errorf("list the roots under %s: %w", prefix, err)
		}
		for _, path := range matches {
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("stat %s: %w", path, err)
			}
			if info.IsDir() {
				roots = append(roots, path)
				continue
			}
			files = append(files, path)
		}
	}

	return roots, files, nil
}

func removeAll(path string) func() error {
	return func() error { return os.RemoveAll(path) }
}
