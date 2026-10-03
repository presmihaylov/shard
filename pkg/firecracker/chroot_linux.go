//go:build linux

package firecracker

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// CheckChrootBase refuses a directory whose mount stops the jailer, which makes device nodes in each chroot under it and execs the vmm from there.
func CheckChrootBase(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	var flags []string
	if st.Flags&unix.ST_NODEV != 0 {
		flags = append(flags, "nodev")
	}
	if st.Flags&unix.ST_NOEXEC != 0 {
		flags = append(flags, "noexec")
	}
	if len(flags) == 0 {
		return nil
	}

	return fmt.Errorf("%s is on a mount with %s, and the jailer makes /dev/kvm in each vmm's chroot under it and execs the vmm from there: remount it without them or use another root", dir, strings.Join(flags, " and "))
}
