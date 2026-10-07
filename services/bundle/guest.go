package bundle

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// userDatabases are the files a user lookup reads, which runc refuses to find as anything but regular files (SHARD-653).
var userDatabases = []string{"etc/passwd", "etc/group"}

// ReadGuest reads one guest file by its path under the top; a path that leads nowhere is fs.ErrNotExist, and a file that is not regular answers its mode alone.
type ReadGuest func(rel string) ([]byte, fs.FileMode, error)

// GuestPaths are the files a create checks in the guest: the user databases when it names a user, then the CA bundle candidates.
func GuestPaths(env []string, user string) []string {
	if user == "" {
		return rootCandidates(env)
	}

	return append(slices.Clone(userDatabases), rootCandidates(env)...)
}

// GuestTree copies the guest's files at paths into a temp tree, so the checks that read an image read the sandbox's own files; the caller removes it.
func GuestTree(paths []string, read ReadGuest) (string, error) {
	tree, err := os.MkdirTemp("", "shard-guest-")
	if err != nil {
		return "", fmt.Errorf("make a tree for the guest's files: %w", err)
	}
	if err := copyGuest(tree, paths, read); err != nil {
		return "", errors.Join(err, os.RemoveAll(tree))
	}

	return tree, nil
}

func copyGuest(tree string, paths []string, read ReadGuest) error {
	for _, rel := range paths {
		data, mode, err := read(rel)
		if unreachable(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !mode.IsRegular() && slices.Contains(userDatabases, rel) {
			return notRegular(rel, mode)
		}
		// A CA bundle that is no file is no bundle, as the image read finds it.
		if !mode.IsRegular() {
			continue
		}

		full := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return fmt.Errorf("make the guest's /%s in %s: %w", filepath.Dir(rel), tree, err)
		}
		if err := os.WriteFile(full, data, 0o600); err != nil {
			return fmt.Errorf("copy the guest's /%s into %s: %w", rel, tree, err)
		}
	}

	return nil
}

// layeredTree is GuestTree over overlay layers, top first, read as the merged view shows them.
func layeredTree(layers, paths []string) (string, error) {
	roots := make([]*os.Root, 0, len(layers))
	for _, layer := range layers {
		root, err := os.OpenRoot(layer)
		if err != nil {
			return "", fmt.Errorf("open the layer %s: %w", layer, err)
		}
		defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush
		roots = append(roots, root)
	}

	return GuestTree(paths, func(rel string) ([]byte, fs.FileMode, error) {
		root, resolved, mode, err := layeredPath(roots, rel)
		if err != nil || !mode.IsRegular() {
			return nil, mode, err
		}
		data, err := readRegular(root, resolved)

		return data, mode, err
	})
}

// readRegular reads a file the open proves regular on its own handle, in one bounded read.
func readRegular(root *os.Root, rel string) ([]byte, error) {
	full := filepath.Join(root.Name(), rel)
	f, err := openDatabase(root, rel, full)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, MaxGuestFile+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", full, err)
	}
	if len(data) > MaxGuestFile {
		return nil, fmt.Errorf("the guest's /%s is over %d MiB, more than shard reads of a guest file", rel, MaxGuestFile>>20)
	}

	return data, nil
}
