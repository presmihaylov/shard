package sysboxrunc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxLinks is what the kernel allows a path before ELOOP, which is the ceiling here too.
const maxLinks = 40

// LookupError is a command the container cannot start, found out on the host before anything ran.
type LookupError struct {
	// Reason reads like a shell's: what was looked for and why it is unusable.
	Reason string
	// NotExecutable separates a command that is there and cannot run from one that is not there.
	NotExecutable bool
}

func (e *LookupError) Error() string { return e.Reason }

// LookPath finds the command as the guest's execve would, inside rootfs: a slash means from workDir,
// a bare name is searched on pathEnv, symlinks resolve inside the tree. It answers like a shell: not
// found, or not executable. A found file can still fail to run (missing interpreter) and keeps exit 1.
func LookPath(rootfs, workDir, pathEnv, name string) error {
	if name == "" {
		return &LookupError{Reason: "empty command"}
	}

	if strings.Contains(name, "/") {
		return lookAt(rootfs, resolveFrom(workDir, name), name)
	}

	var notExecutable *LookupError
	for dir := range strings.SplitSeq(pathEnv, ":") {
		if dir == "" {
			continue
		}

		err := lookAt(rootfs, resolveFrom(workDir, path.Join(dir, name)), name)
		if err == nil {
			return nil
		}

		// A match that cannot run is what a shell answers with when nothing later on PATH can.
		var lookup *LookupError
		if errors.As(err, &lookup) && lookup.NotExecutable && notExecutable == nil {
			notExecutable = lookup
		}
	}

	if notExecutable != nil {
		return notExecutable
	}

	return &LookupError{Reason: fmt.Sprintf("%s: not found", name)}
}

// resolveFrom makes a guest path absolute the way the kernel does for a relative execve: from the cwd.
func resolveFrom(workDir, name string) string {
	if path.IsAbs(name) {
		return path.Clean(name)
	}
	if workDir == "" {
		workDir = "/"
	}

	return path.Join(workDir, name)
}

// lookAt says whether the guest path names a regular file some uid may execute. The mode check is what
// a shell means by 126: a finer answer needs the guest's uid and groups, which runc applies later.
func lookAt(rootfs, guestPath, name string) error {
	hostPath, err := walk(rootfs, guestPath)
	if err != nil {
		return err
	}

	info, err := os.Lstat(hostPath)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return &LookupError{Reason: fmt.Sprintf("%s: not found", name)}
	}
	if err != nil {
		return fmt.Errorf("look for %s in the container: %w", name, err)
	}

	if info.IsDir() {
		return &LookupError{Reason: fmt.Sprintf("%s: is a directory", name), NotExecutable: true}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return &LookupError{Reason: fmt.Sprintf("%s: permission denied", name), NotExecutable: true}
	}

	return nil
}

// walk follows guestPath component by component under rootfs, reading every symlink against the
// tree and never the host, so /bin -> /usr/bin lands in the container's /usr/bin. It returns the host
// path of the final component, which may not exist; a missing directory on the way is not found.
func walk(rootfs, guestPath string) (string, error) {
	rest := strings.Split(strings.Trim(path.Clean(guestPath), "/"), "/")
	current := "/"
	links := 0

	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]

		// path.Join collapses a ".." that would climb out of the root, which is what a chroot does too.
		next := path.Join(current, name)
		info, err := os.Lstat(filepath.Join(rootfs, next))
		if errors.Is(err, fs.ErrNotExist) {
			if len(rest) > 0 {
				return "", &LookupError{Reason: fmt.Sprintf("%s: not found", guestPath)}
			}

			return filepath.Join(rootfs, next), nil
		}
		if err != nil {
			return "", fmt.Errorf("look for %s in the container: %w", guestPath, err)
		}

		if info.Mode()&fs.ModeSymlink == 0 {
			current = next

			continue
		}

		links++
		if links > maxLinks {
			return "", &LookupError{Reason: fmt.Sprintf("%s: too many levels of symbolic links", guestPath)}
		}

		target, err := os.Readlink(filepath.Join(rootfs, next))
		if err != nil {
			return "", fmt.Errorf("read the link %s in the container: %w", next, err)
		}

		// The target's components go back on the queue, in front of what is left of the path.
		rest = append(strings.Split(strings.Trim(path.Clean(target), "/"), "/"), rest...)
		if path.IsAbs(target) {
			current = "/"
		}
	}

	return filepath.Join(rootfs, current), nil
}
