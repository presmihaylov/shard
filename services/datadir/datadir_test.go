package datadir

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/mountinfo"
	"github.com/presmihaylov/shard/pkg/reflink"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/pkg/xfs"
)

// plenty is the free space a fake host reports unless a test sets its own.
const plenty int64 = 1 << 40

// fakeHost records what a bootstrap did to the machine and answers the probe from a script.
type fakeHost struct {
	probes    []reflink.Filesystem
	mounted   bool
	noMkfs    bool
	user      bool
	formatted bool
	existing  int64
	free      int64
	fstabIn   error
	lockAt    string
	steps     []string
	image     string
	size      int64
	migrated  string
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
		isImage:   func(string) (bool, error) { return f.formatted, nil },
		imageSize: func(string) (int64, error) { return f.existing, nil },
		room:      func(string) (int64, error) { return cmp.Or(f.free, plenty), nil },
		isRoot:    func() bool { return !f.user },
		lock: func(path string) (*store.Lock, error) {
			f.lockAt = path
			return store.TryAcquire(path, 0o600)
		},
		inFstab: func(string, string) (bool, error) {
			f.steps = append(f.steps, "fstab check")
			return false, f.fstabIn
		},
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
		migrate: func(image, point string) error {
			f.steps = append(f.steps, "migrate")
			f.migrated = image + " " + point
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

// A reflink root is already mounted, so Ensure mounts nothing and only repairs an old fstab line that lacks nofail.
func TestEnsureRepairsTheFstabLineOnAReflinkFilesystem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	f := &fakeHost{probes: []reflink.Filesystem{xfsR}, user: true, noMkfs: true}
	if err := ensure(t.Context(), Config{Dir: dir, Provider: Firecracker}, f.host()); err != nil {
		t.Fatalf("xfs with reflink: %v", err)
	}
	if want := []string{"migrate"}; strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Errorf("steps %v, want %v", f.steps, want)
	}
	if want := ImagePath(dir) + " " + dir; f.migrated != want {
		t.Errorf("migrated %q, want %q", f.migrated, want)
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
	if want := []string{"fstab check", "image", "mount shard.xfs shard", "fstab"}; strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Errorf("steps %v, want %v", f.steps, want)
	}
	if f.lockAt != dir+".xfs.lock" {
		t.Errorf("locked %s, want %s", f.lockAt, dir+".xfs.lock")
	}
	if f.image != dir+".xfs" {
		t.Errorf("image at %s, want %s", f.image, dir+".xfs")
	}
	if f.size != maxImageMiB<<20 {
		t.Errorf("image size %d, want the cap %d MiB", f.size, maxImageMiB)
	}
	if !strings.Contains(out.String(), "102400 MiB xfs image") {
		t.Errorf("log %q says nothing of the image", out.String())
	}
}

func TestEnsureSizesTheImageFromTheFreeSpace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		free int64
		want int64
	}{
		{"half of it", 40 << 30, 20 << 30},
		{"the floor exactly", 20 << 30, 10 << 30},
		{"capped", 500 << 30, maxImageMiB << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeHost{probes: []reflink.Filesystem{ext4, xfsR}, free: c.free}
			if err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "d"), Provider: Firecracker}, f.host()); err != nil {
				t.Fatalf("%d bytes free: %v", c.free, err)
			}
			if f.size != c.want {
				t.Errorf("%d bytes free gave an image of %d, want %d", c.free, f.size, c.want)
			}
		})
	}
}

func TestEnsureRefusesAnImageUnderTheFloorBeforeAnyBlock(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shard")
	f := &fakeHost{probes: []reflink.Filesystem{ext4}, free: 19 << 30}
	err := ensure(t.Context(), Config{Dir: dir, Provider: Firecracker}, f.host())
	if err == nil {
		t.Fatal("19 GiB free provisioned an image under the floor")
	}
	for _, want := range []string{dir, "19.0 GiB free", "free space on that disk", "on XFS or Btrfs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q in it", err, want)
		}
	}
	if len(f.steps) != 0 {
		t.Errorf("a refusal ran %v", f.steps)
	}
}

// The image took its space when it was made, so the free space left says nothing about it.
func TestEnsureKeepsAnImageThatExistsOnAFullDisk(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{ext4, xfsR}, formatted: true, free: 1 << 30}
	if err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "shard"), Provider: Firecracker}, f.host()); err != nil {
		t.Fatalf("an existing image on a full disk: %v", err)
	}
	if want := []string{"fstab check", "image", "mount shard.xfs shard", "fstab"}; strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Errorf("steps %v, want %v", f.steps, want)
	}
}

func TestEnsureTakesTheConfiguredSize(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{ext4, xfsR}, free: 20 << 30}
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
	h.isImage = func(image string) (bool, error) {
		return false, fmt.Errorf("%s: %w", image, xfs.ErrNotImage)
	}
	err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "d"), Provider: Firecracker}, h)
	if err == nil || !strings.Contains(err.Error(), "not an xfs image: remove it or move the data dir") {
		t.Errorf("got %v", err)
	}
	if len(f.steps) != 0 {
		t.Errorf("a file that is no image ran %v", f.steps)
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

func TestEnsureRefusesAForeignFstabLineBeforeAnyBlock(t *testing.T) {
	t.Parallel()

	f := &fakeHost{probes: []reflink.Filesystem{ext4}, fstabIn: fmt.Errorf("/etc/fstab: %w: %q", xfs.ErrFstabConflict, "/dev/sdb1 /x ext4 defaults 0 2")}
	err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "shard"), Provider: Firecracker}, f.host())
	if !errors.Is(err, xfs.ErrFstabConflict) {
		t.Fatalf("got %v", err)
	}
	if want := []string{"fstab check"}; strings.Join(f.steps, ",") != strings.Join(want, ",") {
		t.Errorf("steps %v, want %v", f.steps, want)
	}
}

func TestEnsureWaitsBehindAnotherBootstrap(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shard")
	held, err := store.TryAcquire(dir+".xfs.lock", 0o600)
	if err != nil || held == nil {
		t.Fatalf("take the lock first: %v, %v", held, err)
	}
	defer held.Release()

	f := &fakeHost{probes: []reflink.Filesystem{ext4}}
	h := f.host()
	h.lock = func(path string) (*store.Lock, error) { return store.Acquire(path, 0o600, 50*time.Millisecond) }
	err = ensure(t.Context(), Config{Dir: dir, Provider: Firecracker}, h)
	if err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("got %v", err)
	}
	if len(f.steps) != 0 {
		t.Errorf("ran %v under another bootstrap", f.steps)
	}
}

func TestEnsureRefusesASizeTheHostCannotSpareBeforeAnyBlock(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "shard")
	f := &fakeHost{probes: []reflink.Filesystem{ext4}, free: 30 << 30}
	err := ensure(t.Context(), Config{Dir: dir, Provider: Firecracker, ImageMiB: 25 << 10}, f.host())
	for _, want := range []string{dir, "cannot take 25 GiB", "only 30.0 GiB is available", "the most it can reserve is 20 GiB"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want %q in it", err, want)
		}
	}
	if len(f.steps) != 0 {
		t.Errorf("a refusal ran %v", f.steps)
	}
}

func TestEnsureRefusesASizeWhereNoImageIsMade(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		provider string
		fs       reflink.Filesystem
		want     string
	}{
		{"another provider", "gvisor", ext4, "gvisor reserves no space"},
		{"a root that clones", Firecracker, xfsR, "xfs, which clones a disk"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeHost{probes: []reflink.Filesystem{c.fs}}
			err := ensure(t.Context(), Config{Dir: t.TempDir(), Provider: c.provider, ImageMiB: MinImageMiB}, f.host())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q in it", err, c.want)
			}
			if len(f.steps) != 0 {
				t.Errorf("a refusal ran %v", f.steps)
			}
		})
	}
}

// The unit setup writes passes the image's own size on every start, so that size starts the daemon and any other refuses before a step.
func TestEnsureHoldsAnExistingImageToItsOwnSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		probes []reflink.Filesystem
		mib    int64
		want   string
		steps  []string
	}{
		{"mounted, its own size", []reflink.Filesystem{xfsR}, 18 << 10, "", []string{"migrate"}},
		{"mounted, another size", []reflink.Filesystem{xfsR}, 20 << 10, "already reserves 18 GiB", nil},
		{"unmounted, its own size", []reflink.Filesystem{ext4, xfsR}, 18 << 10, "", []string{"fstab check", "image", "mount shard.xfs shard", "fstab"}},
		{"unmounted, another size", []reflink.Filesystem{ext4}, 12 << 10, "already reserves 18 GiB", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeHost{probes: c.probes, formatted: true, existing: 18 << 30}
			err := ensure(t.Context(), Config{Dir: filepath.Join(t.TempDir(), "shard"), Provider: Firecracker, ImageMiB: c.mib}, f.host())
			if c.want == "" && err != nil {
				t.Fatalf("got %v, want the image kept", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "set the storage size to 18 GiB, or leave it unset")) {
				t.Errorf("got %v, want %q and the size to set", err, c.want)
			}
			if strings.Join(f.steps, ",") != strings.Join(c.steps, ",") {
				t.Errorf("steps %v, want %v", f.steps, c.steps)
			}
		})
	}
}

func TestCheckSizeHoldsTheMinimumAndTheHostReserve(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mib  int64
		room int64
		want string
	}{
		{"the minimum", 10 << 10, 20 << 30, ""},
		{"all but the reserve", 90 << 10, 100 << 30, ""},
		{"below the minimum", 9 << 10, 100 << 30, "9 GiB is below the minimum of 10 GiB"},
		{"into the reserve", 91 << 10, 100 << 30, "the most it can reserve is 90 GiB"},
		{"no room at all", 10 << 10, 15 << 30, "only 15.0 GiB is available, and shard keeps 10 GiB of it for the host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			err := CheckSize(c.mib, c.room)
			if c.want == "" && err != nil {
				t.Errorf("CheckSize(%d, %d) = %v", c.mib, c.room, err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Errorf("CheckSize(%d, %d) = %v, want %q in it", c.mib, c.room, err, c.want)
			}
		})
	}
}

func TestDefaultAndMaxMiBRoundDownToWholeGiB(t *testing.T) {
	t.Parallel()

	cases := []struct {
		room      int64
		def, most int64
	}{
		{0, 0, 0},
		{(41 << 30) + 1, 20 << 10, 31 << 10},
		{(30 << 30) + (512 << 20), 15 << 10, 20 << 10},
		{1 << 40, maxImageMiB, 1014 << 10},
	}
	for _, c := range cases {
		if got := DefaultMiB(c.room); got != c.def {
			t.Errorf("DefaultMiB(%d) = %d, want %d", c.room, got, c.def)
		}
		if got := MaxMiB(c.room); got != c.most {
			t.Errorf("MaxMiB(%d) = %d, want %d", c.room, got, c.most)
		}
	}
}
