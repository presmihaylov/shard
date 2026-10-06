// Package datadir puts the daemon's root on a filesystem that clones a disk by reference, which Firecracker needs and no other provider does.
package datadir

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/pkg/mountinfo"
	"github.com/presmihaylov/shard/pkg/reflink"
	"github.com/presmihaylov/shard/pkg/size"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/pkg/xfs"
)

// Firecracker is the provider name that asks for a reflink root; SHARD-41 registers the substrate under it.
const Firecracker = "firecracker"

// maxImageMiB caps the default image, which takes half the free space beside the root.
const maxImageMiB int64 = 100 * 1024

// MinImageMiB is the smallest image the daemon provisions; below it the host has no room for a fleet of sandbox disks.
const MinImageMiB int64 = 10 * 1024

// HostReserveMiB is the free space a chosen size leaves the host: what half the free space left it at the smallest image.
const HostReserveMiB int64 = 10 * 1024

// lockWait bounds a second daemon behind a bootstrap in flight, long enough for one mkfs over a full image.
const lockWait = 2 * time.Minute

// Config is what one root needs to be checked or provisioned.
type Config struct {
	Dir      string
	Provider string
	// ImageMiB sizes a new image, and zero takes DefaultMiB; the caller holds it to MinImageMiB, so a test can make a small one.
	ImageMiB int64
	Out      io.Writer
}

// host is every call that touches the machine, so the rules are tested without root or xfsprogs.
type host struct {
	probe     func(string) (reflink.Filesystem, error)
	mounted   func(string) (mountinfo.Mount, bool, error)
	haveMkfs  func() error
	isImage   func(string) (bool, error)
	imageSize func(string) (int64, error)
	room      func(string) (int64, error)
	isRoot    func() bool
	lock      func(string) (*store.Lock, error)
	inFstab   func(string, string) (bool, error)
	makeImage func(context.Context, string, int64) error
	mount     func(context.Context, string, string) error
	fstab     func(string, string) error
	migrate   func(string, string) error
}

var machine = host{
	probe:     reflink.Probe,
	mounted:   mountinfo.At,
	haveMkfs:  xfs.Have,
	isImage:   xfs.IsImage,
	imageSize: fileSize,
	room:      xfs.Room,
	isRoot:    func() bool { return os.Geteuid() == 0 },
	lock:      func(path string) (*store.Lock, error) { return store.Acquire(path, 0o600, lockWait) },
	inFstab:   xfs.InFstab,
	makeImage: xfs.MakeImage,
	mount:     xfs.Mount,
	fstab:     xfs.Fstab,
	migrate:   xfs.MigrateFstab,
}

// Ensure leaves cfg.Dir on a reflink filesystem for Firecracker, and touches nothing for any other provider.
func Ensure(ctx context.Context, cfg Config) error {
	return ensure(ctx, cfg, machine)
}

func ensure(ctx context.Context, cfg Config, h host) (err error) {
	if cfg.Provider != Firecracker && cfg.ImageMiB > 0 {
		return fmt.Errorf("a storage size applies only to Firecracker on a filesystem that cannot clone a disk; %s reserves no space", cfg.Provider)
	}
	if cfg.Provider != Firecracker {
		return nil
	}
	if cfg.ImageMiB < 0 {
		return fmt.Errorf("the data image size cannot be negative, got %d MiB", cfg.ImageMiB)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", cfg.Dir, err)
	}
	// The bootstrap runs before daemon.lock, so this lock, a sibling on the parent filesystem, is what keeps two starts from formatting over each other.
	lock, err := h.lock(ImagePath(cfg.Dir) + ".lock")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	fs, err := h.probe(cfg.Dir)
	if err != nil {
		return err
	}
	// A host provisioned before SHARD-353 is already mounted here, so repair its old fstab line without a reprovision.
	if fs.Reflink {
		if err := noImageToSize(cfg, h, fs); err != nil {
			return err
		}

		return h.migrate(ImagePath(cfg.Dir), cfg.Dir)
	}

	if err := refuse(cfg, h, fs); err != nil {
		return err
	}
	capacity, err := imageSize(cfg, h, fs)
	if err != nil {
		return err
	}

	return provision(ctx, cfg, h, capacity)
}

// refuse names the one thing that stops a bootstrap, before any block is written.
func refuse(cfg Config, h host, fs reflink.Filesystem) error {
	if !h.isRoot() {
		return fmt.Errorf("%s is on %s, which cannot clone a disk, and only root can provision the xfs image over it: run the daemon as root", cfg.Dir, fs.Type)
	}
	if err := h.haveMkfs(); err != nil {
		return fmt.Errorf("%s is on %s, which cannot clone a disk, and the xfs image needs %w", cfg.Dir, fs.Type, err)
	}

	m, found, err := h.mounted(cfg.Dir)
	if err != nil {
		return err
	}
	// A mount already there is one shard did not make, or an image formatted without reflink; neither is ours to replace.
	if found {
		return fmt.Errorf("%s is already a %s mount that cannot clone a disk: unmount it, or move the data dir", cfg.Dir, m.FSType)
	}

	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfg.Dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is on %s, which cannot clone a disk, and it already holds %d entries the xfs mount would hide: move them aside, or start with an empty data dir", cfg.Dir, fs.Type, len(entries))
	}

	return nil
}

// noImageToSize refuses a storage size on a root that clones by itself, where Firecracker makes no image; a root mounted from one holds it to that image's size.
func noImageToSize(cfg Config, h host, fs reflink.Filesystem) error {
	if cfg.ImageMiB == 0 {
		return nil
	}
	formatted, err := h.isImage(ImagePath(cfg.Dir))
	if err != nil {
		return err
	}
	if formatted {
		return sameSize(cfg, h)
	}

	return fmt.Errorf("%s is on %s, which clones a disk, so Firecracker makes no data image there and a storage size does not apply", cfg.Dir, fs.Type)
}

// imageSize is what a new image gets, in bytes: the size asked for, else DefaultMiB; zero keeps the image already there, at its size.
func imageSize(cfg Config, h host, fs reflink.Filesystem) (int64, error) {
	image := ImagePath(cfg.Dir)
	formatted, err := h.isImage(image)
	if errors.Is(err, xfs.ErrNotImage) {
		return 0, fmt.Errorf("%w: remove it or move the data dir", err)
	}
	if err != nil {
		return 0, err
	}
	if formatted {
		return 0, sameSize(cfg, h)
	}

	// Read here, right before the allocation, since the free space can shrink after setup checked it.
	room, err := h.room(image)
	if err != nil {
		return 0, err
	}
	if cfg.ImageMiB > 0 {
		if err := checkRoom(cfg.ImageMiB, room); err != nil {
			return 0, fmt.Errorf("%s is on %s, which cannot clone a disk, and the xfs image beside it cannot take %s: %w", cfg.Dir, fs.Type, size.Show(cfg.ImageMiB), err)
		}

		return cfg.ImageMiB << 20, nil
	}
	mib := DefaultMiB(room)
	if mib < MinImageMiB {
		return 0, fmt.Errorf("%s is on %s, which cannot clone a disk, and the xfs image beside it takes half the free space, at least %d GiB, but %s has %.1f GiB free: free space on that disk, or put %s on XFS or Btrfs", cfg.Dir, fs.Type, MinImageMiB>>10, filepath.Dir(image), float64(room)/(1<<30), cfg.Dir)
	}

	return mib << 20, nil
}

// sameSize refuses a storage size other than the existing image's, since setup's unit passes that size on every start; a part MiB rounds up.
func sameSize(cfg Config, h host) error {
	if cfg.ImageMiB == 0 {
		return nil
	}
	image := ImagePath(cfg.Dir)
	bytes, err := h.imageSize(image)
	if err != nil {
		return err
	}
	if have := (bytes + size.MiB - 1) / size.MiB; have != cfg.ImageMiB {
		return fmt.Errorf("the data image %s already reserves %s, and shard does not resize it: set the storage size to %s, or leave it unset", image, size.Show(have), size.Show(have))
	}

	return nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("read the size of %s: %w", path, err)
	}

	return info.Size(), nil
}

// DefaultMiB is the image a host with room bytes free gets when nobody chose: half the room in whole GiB, at most 100 GiB.
func DefaultMiB(room int64) int64 {
	return min(room>>31<<10, maxImageMiB)
}

// CheckSize refuses a chosen image of mib below MinImageMiB, or one larger than MaxMiB of room bytes.
func CheckSize(mib, room int64) error {
	if mib < MinImageMiB {
		return fmt.Errorf("%s is below the minimum of %s", size.Show(mib), size.Show(MinImageMiB))
	}

	return checkRoom(mib, room)
}

// MaxMiB is the largest image room bytes take while the host keeps HostReserveMiB, in whole GiB.
func MaxMiB(room int64) int64 {
	return max(room>>20-HostReserveMiB, 0) >> 10 << 10
}

func checkRoom(mib, room int64) error {
	most := MaxMiB(room)
	if mib <= most {
		return nil
	}
	if most < MinImageMiB {
		return fmt.Errorf("only %s is available, and shard keeps %s of it for the host", gib(room), size.Show(HostReserveMiB))
	}

	return fmt.Errorf("only %s is available, and shard keeps %s of it for the host, so the most it can reserve is %s", gib(room), size.Show(HostReserveMiB), size.Show(most))
}

func gib(bytes int64) string { return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30)) }

// provision makes the image beside the dir, mounts it there and makes the mount survive a reboot; every step skips what is already done.
func provision(ctx context.Context, cfg Config, h host, capacity int64) error {
	image := ImagePath(cfg.Dir)
	logger := log.New(cmp.Or[io.Writer](cfg.Out, io.Discard), "", log.LstdFlags)

	// A foreign fstab line refuses here, before a mount a retry would then take for a finished bootstrap.
	if _, err := h.inFstab(image, cfg.Dir); err != nil {
		return err
	}
	if capacity > 0 {
		logger.Printf("data dir %s gets a %d MiB xfs image at %s", cfg.Dir, capacity>>20, image)
	}
	if err := h.makeImage(ctx, image, capacity); err != nil {
		return err
	}
	if err := h.mount(ctx, image, cfg.Dir); err != nil {
		return err
	}
	if err := h.fstab(image, cfg.Dir); err != nil {
		return err
	}

	fs, err := h.probe(cfg.Dir)
	if err != nil {
		return err
	}
	if !fs.Reflink {
		return fmt.Errorf("%s is mounted from %s and still cannot clone a disk", cfg.Dir, image)
	}
	logger.Printf("data dir %s is the xfs image at %s, with reflink", cfg.Dir, image)

	return nil
}

// ImagePath is the loopback image of a data dir: a sibling, so it sits on the parent filesystem and never inside its own mount.
func ImagePath(dir string) string {
	return filepath.Clean(dir) + ".xfs"
}
