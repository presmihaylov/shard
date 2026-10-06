package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// CheckWorkDir refuses a workdir the live tree holds as something other than a directory, which runsc exec would run in anyway (SHARD-769).
func (b Bundle) CheckWorkDir(dir string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}
	mounts := make([]string, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		mounts = append(mounts, strings.TrimPrefix(path.Clean(m.Destination), "/"))
	}

	root, err := os.OpenRoot(b.RootFS)
	if err != nil {
		return fmt.Errorf("open the rootfs %s: %w", b.RootFS, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	mode, err := guestMode(root, strings.TrimPrefix(dir, "/"), mounts)
	if errors.Is(err, syscall.ENOTDIR) || err == nil && !mode.IsDir() {
		return &fs.PathError{Op: "chdir", Path: dir, Err: syscall.ENOTDIR}
	}
	// A mount, a missing part or a loop is the runtime's to judge, and runsc already refuses the last two.
	if errors.Is(err, errMounted) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check the workdir %s: %w", dir, err)
	}

	return nil
}
