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

	"github.com/presmihaylov/shard/pkg/mountinfo"
	"github.com/presmihaylov/shard/pkg/reflink"
	"github.com/presmihaylov/shard/pkg/xfs"
)

// Firecracker is the provider name that asks for a reflink root; SHARD-41 registers the substrate under it.
const Firecracker = "firecracker"

// DefaultImageMiB sizes the loopback image when the install set none.
const DefaultImageMiB int64 = 100 * 1024

// Config is what one root needs to be checked or provisioned.
type Config struct {
	Dir      string
	Provider string
	// ImageMiB is the loopback image size; zero takes DefaultImageMiB.
	ImageMiB int64
	Out      io.Writer
}

// host is every call that touches the machine, so the rules are tested without root or xfsprogs.
type host struct {
	probe     func(string) (reflink.Filesystem, error)
	mounted   func(string) (mountinfo.Mount, bool, error)
	haveMkfs  func() error
	isRoot    func() bool
	makeImage func(context.Context, string, int64) error
	mount     func(context.Context, string, string) error
	fstab     func(string, string) error
}

var machine = host{
	probe:     reflink.Probe,
	mounted:   mountinfo.At,
	haveMkfs:  xfs.Have,
	isRoot:    func() bool { return os.Geteuid() == 0 },
	makeImage: xfs.MakeImage,
	mount:     xfs.Mount,
	fstab:     xfs.Fstab,
}

// Ensure leaves cfg.Dir on a reflink filesystem for Firecracker, and touches nothing for any other provider.
func Ensure(ctx context.Context, cfg Config) error {
	return ensure(ctx, cfg, machine)
}

func ensure(ctx context.Context, cfg Config, h host) error {
	if cfg.Provider != Firecracker {
		return nil
	}
	if cfg.ImageMiB < 0 {
		return fmt.Errorf("the data image size cannot be negative, got %d MiB", cfg.ImageMiB)
	}
	if cfg.ImageMiB == 0 {
		cfg.ImageMiB = DefaultImageMiB
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", cfg.Dir, err)
	}

	fs, err := h.probe(cfg.Dir)
	if err != nil {
		return err
	}
	if fs.Reflink {
		return nil
	}

	if err := refuse(cfg, h, fs); err != nil {
		return err
	}

	return provision(ctx, cfg, h)
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

// provision makes the image beside the dir, mounts it there and makes the mount survive a reboot; every step skips what is already done.
func provision(ctx context.Context, cfg Config, h host) error {
	image := ImagePath(cfg.Dir)
	logger := log.New(cmp.Or[io.Writer](cfg.Out, io.Discard), "", log.LstdFlags)

	if err := h.makeImage(ctx, image, cfg.ImageMiB<<20); err != nil {
		if errors.Is(err, xfs.ErrNotImage) {
			return fmt.Errorf("%w: remove it or move the data dir", err)
		}

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
	logger.Printf("data dir %s is a %d MiB xfs image at %s, with reflink", cfg.Dir, cfg.ImageMiB, image)

	return nil
}

// ImagePath is the loopback image of a data dir: a sibling, so it sits on the parent filesystem and never inside its own mount.
func ImagePath(dir string) string {
	return filepath.Clean(dir) + ".xfs"
}
