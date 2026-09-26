package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/services/supervisor"
)

// bootGuest moves PID 1 from the initrd onto the root disk, with the kernel filesystems carried across.
func bootGuest(boot guestBoot) error {
	for _, m := range []struct{ source, target, fstype string }{
		{"devtmpfs", "/dev", "devtmpfs"},
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
	} {
		if err := mountOnce(m.source, m.target, m.fstype, 0); err != nil {
			return err
		}
	}

	if err := mountRoot(boot); err != nil {
		return err
	}
	for _, dir := range []string{"dev", "proc", "sys"} {
		if err := os.MkdirAll("/newroot/"+dir, 0o755); err != nil { //nolint:gosec // a mount point every guest process must traverse
			return err
		}
		if err := unix.Mount("/"+dir, "/newroot/"+dir, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move /%s onto the root disk: %w", dir, err)
		}
	}
	// An exec with a tty needs a pty, which the image's /dev has no pts under.
	if err := os.MkdirAll("/newroot/dev/pts", 0o755); err != nil { //nolint:gosec // a mount point every guest process must traverse
		return err
	}
	if err := mountOnce("devpts", "/newroot/dev/pts", "devpts", 0); err != nil {
		return err
	}
	// A container runtime inside the guest asserts these at start: dockerd refuses to run without a writable cgroup2 root and a /dev/shm.
	for _, m := range []struct{ source, target, fstype string }{
		{"tmpfs", "/newroot/dev/shm", "tmpfs"},
		{"mqueue", "/newroot/dev/mqueue", "mqueue"},
		{"cgroup2", "/newroot/sys/fs/cgroup", "cgroup2"},
	} {
		if err := mountOnce(m.source, m.target, m.fstype, unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC); err != nil {
			return err
		}
	}

	if err := os.Chdir("/newroot"); err != nil {
		return err
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move the root disk onto /: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot onto the root disk: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := boundMemory(); err != nil {
		return err
	}

	if boot.Console == "" {
		return nil
	}
	// The console is hvc0 on the VZ kernel and ttyS0 on Firecracker; /dev/console is a sink on VZ (docs/provider-vz.md).
	console, err := os.OpenFile(boot.Console, os.O_WRONLY, 0)
	if err == nil {
		if err := unix.Dup2(int(console.Fd()), 2); err != nil {
			return fmt.Errorf("put stderr on the console: %w", err)
		}
	}

	return nil
}

// mountRoot lays the root under /newroot: one disk as it is, or an overlay whose lower is the EROFS image and whose upper sits on the second disk.
func mountRoot(boot guestBoot) error {
	if err := os.MkdirAll("/newroot", 0o755); err != nil { //nolint:gosec // a mount point every guest process must traverse
		return err
	}
	if boot.Root != "" {
		if err := unix.Mount(boot.Root, "/newroot", "ext4", 0, ""); err != nil {
			return fmt.Errorf("mount %s on /newroot: %w", boot.Root, err)
		}

		return nil
	}

	// The two mounts stay in the initramfs root, which the pivot leaves unreachable but overlayfs keeps pinned.
	if err := os.MkdirAll("/base", 0o755); err != nil { //nolint:gosec // a mount point every guest process must traverse
		return err
	}
	if err := unix.Mount(boot.Base, "/base", "erofs", unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount %s on /base: %w", boot.Base, err)
	}
	if err := os.MkdirAll("/overlay", 0o755); err != nil { //nolint:gosec // a mount point every guest process must traverse
		return err
	}
	if err := unix.Mount(boot.Overlay, "/overlay", "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s on /overlay: %w", boot.Overlay, err)
	}
	// The guest lays the upper and work directories itself, so the host and shard-init share no name for them.
	for _, dir := range []string{"/overlay/upper", "/overlay/work"} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the upper is the root every guest process traverses
			return err
		}
	}
	if err := unix.Mount("overlay", "/newroot", "overlay", 0, "lowerdir=/base,upperdir=/overlay/upper,workdir=/overlay/work"); err != nil {
		return fmt.Errorf("mount the overlay of %s over %s on /newroot: %w", boot.Overlay, boot.Base, err)
	}

	return nil
}

func mountOnce(source, target, fstype string, flags uintptr) error {
	if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // a mount point every guest process must traverse
		return err
	}
	err := unix.Mount(source, target, fstype, flags, "")
	if err != nil && !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("mount %s on %s: %w", fstype, target, err)
	}

	return nil
}

// confine takes CAP_SYS_PTRACE out of the bounding set, so no guest process can open PID 1's fds, and
// puts the guest in its own cgroup namespace, so a runtime inside it makes cgroups under the sandbox's bound.
func confine() error {
	if os.Getenv(capbsetEnv) == "" {
		// The bounding set and the namespace are per thread, and exec carries the calling thread's, so a re-exec gives them to the whole runtime.
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, unix.CAP_SYS_PTRACE, 0, 0, 0); err != nil {
			return fmt.Errorf("drop CAP_SYS_PTRACE from the bounding set: %w", err)
		}
		if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
			return fmt.Errorf("unshare the cgroup namespace: %w", err)
		}

		return syscall.Exec("/proc/self/exe", os.Args, append(os.Environ(), capbsetEnv+"=1"))
	}
	// PID 1 stays undumpable, so a root exec cannot read the supervisor's sockets through /proc/1/fd.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("clear the dumpable flag: %w", err)
	}

	return remountCgroup()
}

// applyAddress sets the interface by ioctl, as the image has no iproute2 to shell out to.
func applyAddress(a supervisor.Address) error {
	ip := net.ParseIP(a.IP).To4()
	if ip == nil {
		return fmt.Errorf("the address %q is not IPv4", a.IP)
	}
	gateway := net.ParseIP(a.Gateway).To4()
	if gateway == nil {
		return fmt.Errorf("the gateway %q is not IPv4", a.Gateway)
	}
	mask := net.CIDRMask(a.Prefix, 32)
	if mask == nil {
		return fmt.Errorf("the prefix %d is not one of /0 to /32", a.Prefix)
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open the ioctl socket: %w", err)
	}
	defer unix.Close(fd)

	if a.MAC != "" {
		// A fork's interface is already up with the source's MAC, and a live address change is not every driver's; take it down first.
		if err := setFlags(fd, a.Interface, 0); err != nil {
			return err
		}
		if err := setHWAddr(fd, a.Interface, a.MAC); err != nil {
			return err
		}
	}
	for _, step := range []struct {
		request uint
		addr    net.IP
	}{
		{unix.SIOCSIFADDR, ip},
		{unix.SIOCSIFNETMASK, net.IP(mask)},
	} {
		if err := ifreqAddr(fd, a.Interface, step.request, step.addr); err != nil {
			return err
		}
	}

	if err := setFlags(fd, a.Interface, unix.IFF_UP|unix.IFF_RUNNING); err != nil {
		return err
	}

	if err := setDefaultRoute(fd, a.Interface, gateway); err != nil {
		return err
	}

	return writeResolverFiles(a)
}

// writeResolverFiles is what the bundle writes into an upper layer on Linux; a VM's disk is the guest's alone, so the guest writes it.
func writeResolverFiles(a supervisor.Address) error {
	if a.Hostname != "" {
		// runsc sets the hostname from the OCI spec; in a VM the kernel keeps its build-time default until the guest sets one.
		if err := unix.Sethostname([]byte(a.Hostname)); err != nil {
			return fmt.Errorf("set the hostname %q: %w", a.Hostname, err)
		}
	}

	return writeResolverFilesIn("/etc", a)
}

// setFlags writes the interface flags, which brings the link up and, with none, takes it down.
func setFlags(fd int, name string, flags uint16) error {
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	ifr.SetUint16(flags)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("set the flags of %s: %w", name, err)
	}

	return nil
}

func ifreqAddr(fd int, name string, request uint, addr net.IP) error {
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := ifr.SetInet4Addr(addr); err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, request, ifr); err != nil {
		return fmt.Errorf("set %s on %s: %w", addr, name, err)
	}

	return nil
}

// ifreqHWAddr mirrors struct ifreq with ifr_hwaddr in the union, which x/sys reads but never sets; the pad brings it to the 40 bytes the kernel copies.
type ifreqHWAddr struct {
	name   [unix.IFNAMSIZ]byte
	family uint16
	data   [14]byte
	pad    [8]byte
}

// setHWAddr changes the MAC of a down interface, which is what gives a fork its own and leaves the rest of the restored guest alone.
func setHWAddr(fd int, name, mac string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("the mac %q: %w", mac, err)
	}
	if len(hw) != 6 {
		return fmt.Errorf("the mac %q is not 48 bits", mac)
	}
	if len(name) >= unix.IFNAMSIZ {
		return fmt.Errorf("the interface name %q is too long", name)
	}
	ifr := ifreqHWAddr{family: unix.ARPHRD_ETHER}
	copy(ifr.name[:], name)
	copy(ifr.data[:], hw)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCSIFHWADDR, uintptr(unsafe.Pointer(&ifr))) //nolint:gosec // the ifreq ioctl takes a struct pointer
	if errno != 0 {
		return fmt.Errorf("set the mac %s on %s: %w", mac, name, errno)
	}

	return nil
}

// rtentry mirrors struct rtentry from <linux/route.h>, which x/sys does not carry.
type rtentry struct {
	pad1    uint64
	dst     unix.RawSockaddrInet4
	gateway unix.RawSockaddrInet4
	genmask unix.RawSockaddrInet4
	flags   uint16
	pad2    int16
	pad3    uint64
	tos     uint8
	class   uint8
	pad4    [3]int16
	metric  int16
	dev     *byte
	mtu     uint64
	window  uint64
	irtt    uint16
}

// setDefaultRoute replaces the default route with one over gateway; the old one goes first or SIOCADDRT says EEXIST.
func setDefaultRoute(fd int, name string, gateway net.IP) error {
	dev, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	route := rtentry{flags: unix.RTF_UP | unix.RTF_GATEWAY, dev: dev}
	route.dst.Family = unix.AF_INET
	route.genmask.Family = unix.AF_INET
	route.gateway.Family = unix.AF_INET
	copy(route.gateway.Addr[:], gateway)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCDELRT, uintptr(unsafe.Pointer(&route))) //nolint:gosec // the rtentry ioctl takes a struct pointer
	if errno != 0 && !errors.Is(errno, unix.ESRCH) {
		return fmt.Errorf("delete the default route: %w", errno)
	}
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCADDRT, uintptr(unsafe.Pointer(&route))) //nolint:gosec // the rtentry ioctl takes a struct pointer
	if errno != 0 {
		return fmt.Errorf("add the default route over %s: %w", gateway, errno)
	}

	return nil
}

// powerOff ends the VM once the stop is done, by a reboot where the vmm only exits on one; a test process is not PID 1 and just exits.
func powerOff(reboot bool) error {
	if os.Getpid() != 1 {
		return nil
	}
	// The reboot call flushes nothing, and a clone reads the disk: what the guest wrote must reach it first.
	unix.Sync()
	cmd := unix.LINUX_REBOOT_CMD_POWER_OFF
	if reboot {
		cmd = unix.LINUX_REBOOT_CMD_RESTART
	}
	if err := unix.Reboot(cmd); err != nil {
		return fmt.Errorf("power off: %w", err)
	}

	return nil
}
