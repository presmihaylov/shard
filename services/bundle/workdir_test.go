package bundle_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

// live lays the sandbox's live tree as the host sees it: the rootfs, a file and a directory there and links to each, and the init file a bind shows the guest.
func live(t *testing.T) bundle.Bundle {
	t.Helper()

	initPath := filepath.Join(t.TempDir(), "shard-init")
	write(t, initPath, "the supervisor\n")
	svc, err := bundle.New(initPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := svc.Build(runspec.Resolve(newSpec(t), models.ImageConfig{}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, dir := range []string{"etc", "srv/app", "tmp", "proc"} {
		if err := os.MkdirAll(filepath.Join(b.RootFS, dir), 0o755); err != nil {
			t.Fatalf("make /%s: %v", dir, err)
		}
	}
	write(t, filepath.Join(b.RootFS, "etc/hostname"), "sandbox\n")
	write(t, filepath.Join(b.RootFS, "tmp/stale"), "the image's own /tmp, which a bind mount hides\n")
	symlink(t, "/etc/hostname", filepath.Join(b.RootFS, "srv/name"))
	symlink(t, "app", filepath.Join(b.RootFS, "srv/current"))
	symlink(t, bundle.GuestInitPath, filepath.Join(b.RootFS, "srv/init"))

	return b
}

// runsc exec runs a command in a workdir that is a file, bound there or not (SHARD-769).
func TestCheckWorkDirRefusesWhatIsNotADirectory(t *testing.T) {
	for _, dir := range []string{
		"/etc/hostname", "/srv/name", "/etc/hostname/sub", "/etc/hostname/", "/etc/hostname/..",
		bundle.GuestInitPath, bundle.GuestInitPath + "/", bundle.GuestInitPath + "/sub", "/srv/init",
	} {
		t.Run(dir, func(t *testing.T) {
			b := live(t)
			err := b.CheckWorkDir("amber-otter-1a2b", dir)

			refused, ok := errors.AsType[*models.CommandNotStartedError](err)
			want := fmt.Sprintf("the work directory %q is not a directory", dir)
			if !ok || refused.Code != models.CommandNotExecutableExitCode || refused.Reason != want {
				t.Fatalf("the check returned %v, want a command that never started, with 126 and %q", err, want)
			}
			if strings.Contains(err.Error(), b.RootFS) {
				t.Errorf("the refusal %q names the host path %s", err, b.RootFS)
			}
		})
	}
}

// A directory runs, and what lies inside a mount or is not there is the runtime's to judge.
func TestCheckWorkDirPassesADirectoryAndWhatTheHostCannotSee(t *testing.T) {
	for _, dir := range []string{"/", "/etc", "/srv/app", "/srv/current", "/srv/app/../app", "/tmp", "/tmp/stale", "/proc/1", "/.shard", "/.shard/started", "/missing"} {
		t.Run(dir, func(t *testing.T) {
			if err := live(t).CheckWorkDir("amber-otter-1a2b", dir); err != nil {
				t.Fatalf("CheckWorkDir(%q) = %v, want nil", dir, err)
			}
		})
	}
}
