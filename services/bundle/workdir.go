package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/models"
)

// CheckWorkDir refuses a workdir the live tree holds as something other than a directory, which runsc runs in and runc refuses in its own words (SHARD-769).
func (b Bundle) CheckWorkDir(id, dir string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}
	mounts := make([]string, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		mounts = append(mounts, strings.TrimPrefix(path.Clean(m.Destination), "/"))
	}

	return checkWorkDir(id, b.RootFS, mounts, dir)
}

// CheckWorkDirIn is CheckWorkDir over a root that already shows the guest's mounts, as /proc/<pid>/root does.
func CheckWorkDirIn(id, rootfs, dir string) error {
	return checkWorkDir(id, rootfs, nil, dir)
}

// checkWorkDir answers a workdir that is not a directory as a refused execve is answered: the kernel's words and a shell's 126.
func checkWorkDir(id, rootfs string, mounts []string, dir string) error {
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return fmt.Errorf("sandbox %s: open the rootfs %s: %w", id, rootfs, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	mode, err := guestMode(root, strings.TrimPrefix(dir, "/"), mounts)
	if errors.Is(err, syscall.ENOTDIR) || err == nil && !mode.IsDir() {
		return &models.CommandNotStartedError{Sandbox: id, Reason: syscall.ENOTDIR.Error(), Code: models.CommandNotExecutableExitCode}
	}
	// A mount, a missing part or a loop is the runtime's to judge, and every runtime already refuses the last two.
	if errors.Is(err, errMounted) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox %s: check the workdir %s: %w", id, dir, err)
	}

	return nil
}
