package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// live lays the sandbox's live tree as the rootfs mount shows it to the host: a file, a directory and links to each.
func live(t *testing.T) bundle.Bundle {
	t.Helper()

	b := built(t)
	for _, dir := range []string{"etc", "srv/app", "tmp", "proc"} {
		if err := os.MkdirAll(filepath.Join(b.RootFS, dir), 0o755); err != nil {
			t.Fatalf("make /%s: %v", dir, err)
		}
	}
	write(t, filepath.Join(b.RootFS, "etc/hostname"), "sandbox\n")
	write(t, filepath.Join(b.RootFS, "tmp/stale"), "the image's own /tmp, which a bind mount hides\n")
	symlink(t, "/etc/hostname", filepath.Join(b.RootFS, "srv/name"))
	symlink(t, "app", filepath.Join(b.RootFS, "srv/current"))

	return b
}

// runsc exec runs a command in a workdir that is a file, and runc refuses it in its own words before the launch shim (SHARD-769).
func TestCheckWorkDirRefusesWhatIsNotADirectory(t *testing.T) {
	for _, dir := range []string{"/etc/hostname", "/srv/name", "/etc/hostname/sub", "/etc/hostname/", "/etc/hostname/.."} {
		t.Run(dir, func(t *testing.T) {
			b := live(t)
			refusedWorkDir(t, b.CheckWorkDir("amber-otter-1a2b", dir), b.RootFS)
		})
	}
}

func refusedWorkDir(t *testing.T, err error, host string) {
	t.Helper()

	refused, ok := errors.AsType[*models.CommandNotStartedError](err)
	if !ok || refused.Code != models.CommandNotExecutableExitCode || refused.Reason != "not a directory" {
		t.Fatalf("the check returned %v, want a command that never started, with 126 and \"not a directory\"", err)
	}
	if strings.Contains(err.Error(), host) {
		t.Errorf("the refusal %q names the host path %s", err, host)
	}
}

// /proc/<pid>/root shows the guest's mounts, so nothing there is left to the runtime.
func TestCheckWorkDirInJudgesUnderAMountToo(t *testing.T) {
	b := live(t)
	refusedWorkDir(t, bundle.CheckWorkDirIn("amber-otter-1a2b", b.RootFS, "/tmp/stale"), b.RootFS)

	err := bundle.CheckWorkDirIn("amber-otter-1a2b", filepath.Join(t.TempDir(), "gone"), "/")
	if _, ok := errors.AsType[*models.CommandNotStartedError](err); ok || err == nil {
		t.Errorf("a root that cannot be opened gave %v, want the check's own error", err)
	}
}

// A directory runs, and what a guest mount covers or what is not there is the runtime's to judge.
func TestCheckWorkDirPassesADirectoryAndWhatTheHostCannotSee(t *testing.T) {
	for _, dir := range []string{"/", "/etc", "/srv/app", "/srv/current", "/srv/app/../app", "/tmp/stale", "/proc/1", "/missing"} {
		t.Run(dir, func(t *testing.T) {
			if err := live(t).CheckWorkDir("amber-otter-1a2b", dir); err != nil {
				t.Fatalf("CheckWorkDir(%q) = %v, want nil", dir, err)
			}
		})
	}
}
