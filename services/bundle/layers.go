package bundle

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// layeredPath resolves rel as guestPath does, through overlay layers top first, and names the layer whose copy the merged view shows.
func layeredPath(roots []*os.Root, rel string) (*os.Root, string, fs.FileMode, error) {
	parts := strings.Split(rel, "/")
	resolved := ""
	// merged holds, for the top and each directory on the way, the layers whose copy of it shows through.
	merged := [][]*os.Root{roots}
	var holder *os.Root
	mode := fs.ModeDir
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if !mode.IsDir() {
			return nil, "", 0, &fs.PathError{Op: "lstat", Path: resolved, Err: syscall.ENOTDIR}
		}
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = strings.TrimSuffix(filepath.Dir(resolved), ".")
			merged = merged[:max(len(merged)-1, 1)]
			mode = fs.ModeDir
			continue
		}

		next := filepath.Join(resolved, part)
		info, layer, dirs, err := lookupLayers(merged[len(merged)-1], next)
		if err != nil {
			return nil, "", 0, err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			resolved, mode, holder = next, info.Mode(), layer
			if mode.IsDir() {
				merged = append(merged, dirs)
			}
			continue
		}

		links++
		if links > maxLinks {
			return nil, "", 0, &fs.PathError{Op: "lstat", Path: next, Err: syscall.ELOOP}
		}
		target, err := layer.Readlink(next)
		if err != nil {
			return nil, "", 0, err
		}
		if filepath.IsAbs(target) {
			resolved = ""
			merged = merged[:1]
		}
		parts = append(strings.Split(target, "/"), parts...)
	}

	return holder, resolved, mode, nil
}

// lookupLayers finds what the merged view shows at rel among the layers of its parent, and which of them a directory there merges.
func lookupLayers(layers []*os.Root, rel string) (fs.FileInfo, *os.Root, []*os.Root, error) {
	var found fs.FileInfo
	var holder *os.Root
	var dirs []*os.Root
	for _, layer := range layers {
		info, err := layer.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if whiteout(info) {
			break
		}
		if found == nil {
			found, holder = info, layer
		}
		// A directory merges only with the directories below it, and anything else hides what is below.
		if !found.IsDir() || !info.IsDir() {
			break
		}
		dirs = append(dirs, layer)
		opaque, err := opaqueDir(layer, rel)
		if err != nil {
			return nil, nil, nil, err
		}
		if opaque {
			break
		}
	}
	if found == nil {
		return nil, nil, nil, &fs.PathError{Op: "lstat", Path: rel, Err: fs.ErrNotExist}
	}

	return found, holder, dirs, nil
}

// whiteout is the character device 0:0 overlayfs leaves in an upper layer for a file removed from the layers below.
func whiteout(info fs.FileInfo) bool {
	if info.Mode()&fs.ModeCharDevice == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)

	return ok && stat.Rdev == 0
}
