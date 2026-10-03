//go:build linux

package netns

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ChownTap gives the persistent tap name to uid and gid, so a process of theirs attaches to it with no capability; nothing may hold the tap meanwhile.
func ChownTap(name string, uid, gid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	err = chownTap(fd, name, uid, gid)
	if cerr := unix.Close(fd); cerr != nil {
		err = errors.Join(err, fmt.Errorf("close /dev/net/tun: %w", cerr))
	}

	return err
}

func chownTap(fd int, name string, uid, gid int) error {
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("tap %s: %w", name, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("attach to tap %s: %w", name, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, uid); err != nil {
		return fmt.Errorf("give tap %s to uid %d: %w", name, uid, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETGROUP, gid); err != nil {
		return fmt.Errorf("give tap %s to gid %d: %w", name, gid, err)
	}

	return nil
}
