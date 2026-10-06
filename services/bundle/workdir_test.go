package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

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

// runsc exec runs a command in a workdir that is a file, where runc and the kernel refuse with ENOTDIR (SHARD-769).
func TestCheckWorkDirRefusesWhatIsNotADirectory(t *testing.T) {
	for _, dir := range []string{"/etc/hostname", "/srv/name", "/etc/hostname/sub", "/etc/hostname/", "/etc/hostname/.."} {
		t.Run(dir, func(t *testing.T) {
			err := live(t).CheckWorkDir(dir)
			if !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("CheckWorkDir(%q) = %v, want ENOTDIR", dir, err)
			}
		})
	}
}

// A directory runs, and what a guest mount covers or what is not there is runsc's to judge.
func TestCheckWorkDirPassesADirectoryAndWhatTheHostCannotSee(t *testing.T) {
	for _, dir := range []string{"/", "/etc", "/srv/app", "/srv/current", "/srv/app/../app", "/tmp/stale", "/proc/1", "/missing"} {
		t.Run(dir, func(t *testing.T) {
			if err := live(t).CheckWorkDir(dir); err != nil {
				t.Fatalf("CheckWorkDir(%q) = %v, want nil", dir, err)
			}
		})
	}
}
