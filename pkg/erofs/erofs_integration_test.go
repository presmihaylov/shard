//go:build integration

package erofs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// requireTool skips where the image cannot be built, or built and mounted back.
func requireTool(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("mounting the image back needs root")
	}
	if _, err := exec.LookPath(Tool); err != nil {
		t.Skipf("no %s on this host", Tool)
	}
}

func TestBuildMakesAnImageTheKernelMounts(t *testing.T) {
	requireTool(t)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "etc/hostname"), []byte("shard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(filepath.Join(src, "etc/hostname"), 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hostname", filepath.Join(src, "etc/link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "image.erofs")

	if err := Build(t.Context(), dst, src); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := exec.LookPath("fsck.erofs"); err == nil {
		if out, err := exec.Command("fsck.erofs", dst).CombinedOutput(); err != nil {
			t.Fatalf("fsck.erofs: %v\n%s", err, out)
		}
	}
	if runtime.GOOS != "linux" {
		t.Skip("mounting the image back needs Linux")
	}
	mnt := t.TempDir()
	if out, err := exec.Command("mount", "-t", "erofs", "-o", "loop,ro", dst, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			t.Errorf("umount: %v\n%s", err, out)
		}
	}()

	got, err := os.ReadFile(filepath.Join(mnt, "etc/link"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "shard\n" {
		t.Errorf("read %q through the symlink", got)
	}
	info, err := os.Stat(filepath.Join(mnt, "etc/hostname"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 1000 {
		t.Errorf("the owner came back as %+v", info.Sys())
	}
	if err := os.WriteFile(filepath.Join(mnt, "etc/new"), nil, 0o644); err == nil {
		t.Error("the image took a write")
	}
}
