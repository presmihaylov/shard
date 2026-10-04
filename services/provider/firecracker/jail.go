package firecracker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/bundle"
)

// jail makes a fresh jail for the sandbox's next vmm and records it, with the vmm's uid; snap is the snapshot a restore loads, empty for a boot.
func (p *Provider) jail(id, dir string, r *record, snap string) (fcapi.Jail, error) {
	if r.UID == 0 {
		uid, err := p.nextUID()
		if err != nil {
			return fcapi.Jail{}, err
		}
		r.UID = uid
	}
	j := fcapi.Jail{
		Jailer: p.cfg.Jailer, Exec: p.exec, ID: id, UID: r.UID, Base: p.cfg.JailBase,
		Cgroup: strings.TrimPrefix(bundle.CgroupsPath(id), "/"),
	}
	// The network service builds the tap inside a netns named for the sandbox, before every spawn.
	if r.Tap != "" {
		j.Netns = netns.NamespacePath(id)
	}
	r.Jail = j.Root()
	if err := writeRecord(dir, *r); err != nil {
		return fcapi.Jail{}, err
	}

	// The jailer's mknod and pid file refuse what an earlier spawn left, so every spawn gets a jail of its own.
	if err := removeJail(r.Jail); err != nil {
		return fcapi.Jail{}, err
	}
	if err := os.MkdirAll(r.Jail, 0o700); err != nil {
		return fcapi.Jail{}, fmt.Errorf("create the jail: %w", err)
	}
	if err := p.fill(j, dir, *r, snap); err != nil {
		return fcapi.Jail{}, errors.Join(err, removeJail(r.Jail))
	}

	return j, nil
}

// fill puts in the jail what the vmm opens, by reference so no disk or memory is copied; the overlay is a link, so the guest writes where snapshot and pause read.
func (p *Provider) fill(j fcapi.Jail, dir string, r record, snap string) error {
	shared := [][2]string{{jailKernel, p.kernel}, {jailInitrd, p.initrd}, {jailBase, r.BaseDisk}}
	if snap != "" {
		// The memory holds the kernel and the initrd, so a restore takes the snapshot's two files in their place.
		shared = [][2]string{{jailBase, r.BaseDisk}, {jailState, filepath.Join(snap, snapshotState)}, {jailMemory, filepath.Join(snap, memoryFile)}}
	}
	for _, file := range shared {
		if err := bundle.Reflink(file[1], j.Host(file[0])); err != nil {
			return fmt.Errorf("put %s in the jail: %w", file[0], err)
		}
		if err := p.give(j.Host(file[0]), r.UID, 0o400); err != nil {
			return err
		}
	}
	overlay := j.Host(jailOverlay)
	if err := os.Link(filepath.Join(dir, bundle.OverlayDiskFile), overlay); err != nil {
		return fmt.Errorf("link the overlay into the jail: %w", err)
	}
	if err := p.give(overlay, r.UID, 0o600); err != nil {
		return err
	}
	if r.Tap == "" {
		return nil
	}
	// The tun driver lets the tap's owner attach with no capability, which the vmm has none of.
	if err := p.ownTap(j.ID, r.Tap, r.UID, r.UID); err != nil {
		return err
	}

	return nil
}

// give hands a file in the jail to the vmm's uid alone.
func (p *Provider) give(path string, uid int, perm os.FileMode) error {
	if err := p.chown(path, uid, uid); err != nil {
		return fmt.Errorf("give %s to uid %d: %w", filepath.Base(path), uid, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("set the mode of %s: %w", filepath.Base(path), err)
	}

	return nil
}

// removeJail drops the jail of a vmm that is gone, with the directory the jailer named by the id; a vmm from before the jail has none.
func removeJail(jail string) error {
	if jail == "" {
		return nil
	}
	if err := os.RemoveAll(filepath.Dir(jail)); err != nil {
		return fmt.Errorf("remove the jail %s: %w", jail, err)
	}

	return nil
}
