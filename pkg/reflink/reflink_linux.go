//go:build linux

package reflink

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The magics the daemon may meet under its root; anything else is reported in hex.
var names = map[int64]string{
	unix.XFS_SUPER_MAGIC:       "xfs",
	unix.BTRFS_SUPER_MAGIC:     "btrfs",
	unix.EXT4_SUPER_MAGIC:      "ext4",
	unix.TMPFS_MAGIC:           "tmpfs",
	unix.OVERLAYFS_SUPER_MAGIC: "overlay",
	unix.NFS_SUPER_MAGIC:       "nfs",
}

func probe(dir string) (Filesystem, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return Filesystem{}, fmt.Errorf("statfs %s: %w", dir, err)
	}
	fs := Filesystem{Type: names[int64(st.Type)]}
	if fs.Type == "" {
		fs.Type = fmt.Sprintf("%#x", st.Type)
	}

	// A magic says xfs, not whether it was formatted with reflink, so the answer comes from a real clone.
	src, err := os.CreateTemp(dir, ".shard-reflink-*")
	if err != nil {
		return Filesystem{}, fmt.Errorf("probe %s: %w", dir, err)
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		return Filesystem{}, fmt.Errorf("probe %s: %w", dir, err)
	}
	dst := src.Name() + ".clone"
	defer os.Remove(dst)

	err = clone(src.Name(), dst)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
		return fs, nil
	}
	if err != nil {
		return Filesystem{}, err
	}
	fs.Reflink = true

	return fs, nil
}

func clone(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		return errors.Join(fmt.Errorf("clone %s to %s: %w", filepath.Base(src), dst, err), out.Close())
	}

	return out.Close()
}
