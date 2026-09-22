package datadir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/mountinfo"
	"github.com/presmihaylov/shard/pkg/reflink"
	"github.com/presmihaylov/shard/pkg/xfs"
)

// fakeHost records what a bootstrap did to the machine and answers the probe from a script.
type fakeHost struct {
	probes  []reflink.Filesystem
	mounted bool
	noMkfs  bool
	user    bool
	steps   []string
	image   string
	size    int64
}

func (f *fakeHost) host() host {
	return host{
		probe: func(string) (reflink.Filesystem, error) {
			fs := f.probes[0]
			if len(f.probes) > 1 {
				f.probes = f.probes[1:]
			}
			return fs, nil
		},
		mounted: func(string) (mountinfo.Mount, bool, error) {
			return mountinfo.Mount{FSType: "ext4"}, f.mounted, nil
		},
		haveMkfs: func() error {
			if f.noMkfs {
				return errors.New("mkfs.xfs is not on PATH; install xfsprogs")
			}
			return nil
		},
		isRoot: func() bool { return !f.user },
		makeImage: func(_ context.Context, image string, size int64) error {
			f.steps = append(f.steps, "image")
			f.image, f.size = image, size
			return nil
		},
		mount: func(_ context.Context, image, point string) error {
			f.steps = append(f.steps, "mount "+filepath.Base(image)+" "+filepath.Base(point))
			return nil
		},
		fstab: func(string, string) error {
			f.steps = append(f.steps, "fstab")
			return nil
		},
	}
}

var (
	ext4 = reflink.Filesystem{Type: "ext4"}
	xfsR = reflink.Filesystem{Type: "xfs", Reflink: true}
)

func TestEnsureTouchesNothingForAnotherProvider(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "missing")
	f := &fakeHost{probes: []reflink.Filesystem{ext4}, user: true}
	if err := ensure(t.Context(), Config{Dir: dir, Provider: "gvisor"}, f.host()); err != nil {
		t.Fatalf("gvisor: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gvisor created %s", dir)
	}
	if len(f.steps) != 0 {
		t.Errorf("gvisor ran %v", f.steps)
	}
}

func TestEnsureIsANoOpOnAReflinkFilesystem(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{xfsR}, user: true, noMkfs: true}
	if err := ensure(t.Context(), Config{Dir: t.TempDir(), Provider: Firecracker}, f.host()); err != nil {
		t.Fatalf("xfs with reflink: %v", err)
	}
	if len(f.steps) != 0 {
		t.Errorf("ran %v on a reflink root", f.steps)
	}
}

func TestEnsureProvisionsAnImageBesideAnEmptyDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shard")
	var out bytes.Buffer
	f := &fakeHost{probes: []reflink.Filesystem{ext4, xfsR}}
	if err := ensure(t.Context(), Config{Dir: dir, Provider: Firecracker, Out: &out}, f.host()); err != nil {
		t.Fatalf("ext4: %v", err)
	}
	if want := []string{"image", "mount shard.xfs shard", "fstab"}; strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Errorf("steps %v, want %v", f.steps, want)
	}
	if f.image != dir+".xfs" {
		t.Errorf("image at %s, want %s", f.image, dir+".xfs")
	}
	if f.size != DefaultImageMiB<<20 {
		t.Errorf("image size %d, want the default %d MiB", f.size, DefaultImageMiB)
	}
	if !strings.Contains(out.String(), "102400 MiB xfs image") {
		t.Errorf("log %q says nothing of the image", out.String())
	}
}

func TestEnsureTakesTheConfiguredSize(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{ext4, xfsR}}
	if err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "d"), Provider: Firecracker, ImageMiB: 512}, f.host()); err != nil {
		t.Fatalf("512 MiB: %v", err)
	}
	if f.size != 512<<20 {
		t.Errorf("image size %d, want %d", f.size, 512<<20)
	}

	err := ensure(t.Context(), Config{Dir: t.TempDir(), Provider: Firecracker, ImageMiB: -1}, f.host())
	if err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Errorf("-1 MiB got %v", err)
	}
}

func TestEnsureRefusesWithTheFix(t *testing.T) {
	t.Parallel()

	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "daemon.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		dir  string
		host *fakeHost
		want string
	}{
		{"not root", t.TempDir(), &fakeHost{user: true}, "run the daemon as root"},
		{"no xfsprogs", t.TempDir(), &fakeHost{noMkfs: true}, "install xfsprogs"},
		{"a mount", t.TempDir(), &fakeHost{mounted: true}, "already a ext4 mount"},
		{"holds data", full, &fakeHost{}, "1 entries the xfs mount would hide"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			c.host.probes = []reflink.Filesystem{ext4}
			err := ensure(t.Context(), Config{Dir: c.dir, Provider: Firecracker}, c.host.host())
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.dir) {
				t.Errorf("got %v, want the dir and %q", err, c.want)
			}
			if len(c.host.steps) != 0 {
				t.Errorf("a refusal ran %v", c.host.steps)
			}
		})
	}
}

func TestEnsureNamesAFileThatIsNoImage(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{ext4}}
	h := f.host()
	h.makeImage = func(_ context.Context, image string, _ int64) error {
		return fmt.Errorf("%s: %w", image, xfs.ErrNotImage)
	}
	err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "d"), Provider: Firecracker}, h)
	if err == nil || !strings.Contains(err.Error(), "not an xfs image: remove it or move the data dir") {
		t.Errorf("got %v", err)
	}
}

func TestEnsureFailsWhenTheMountStillCannotClone(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{ext4, {Type: "xfs"}}}
	err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "d"), Provider: Firecracker}, f.host())
	if err == nil || !strings.Contains(err.Error(), "still cannot clone a disk") {
		t.Errorf("got %v", err)
	}
}
