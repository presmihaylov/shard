package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/launch"
)

// CheckWorkDir refuses a workdir the live tree holds as something other than a directory, which runsc runs in (SHARD-769).
func (b Bundle) CheckWorkDir(id, dir string) error {
	spec, err := b.readSpec()
	if err != nil {
		return err
	}
	// A bind keeps its host source, and any other mount maps to "": the host cannot look into it.
	mounts := make(map[string]string, len(spec.Mounts))
	for _, m := range spec.Mounts {
		source := ""
		if m.Type == "bind" || slices.Contains(m.Options, "bind") || slices.Contains(m.Options, "rbind") {
			source = m.Source
		}
		mounts[strings.TrimPrefix(path.Clean(m.Destination), "/")] = source
	}

	root, err := os.OpenRoot(b.RootFS)
	if err != nil {
		return fmt.Errorf("sandbox %s: open the rootfs %s: %w", id, b.RootFS, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	_, mode, err := guestPath(root, strings.TrimPrefix(dir, "/"), mounts)
	if errors.Is(err, syscall.ENOTDIR) || err == nil && !mode.IsDir() {
		return &models.CommandNotStartedError{Sandbox: id, Reason: launch.WorkDirReason(dir, syscall.ENOTDIR), Code: models.CommandNotExecutableExitCode}
	}
	// A path inside a mount, a missing part or a loop is the runtime's to judge, and runsc already refuses the last two.
	if errors.Is(err, errMounted) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox %s: check the workdir %s: %w", id, dir, err)
	}

	return nil
}
