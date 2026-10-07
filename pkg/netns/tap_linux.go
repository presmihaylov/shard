//go:build linux

package netns

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// ChownTapIn gives the persistent tap name inside the namespace to uid and gid, so a process of theirs attaches to it with no capability; nothing may hold the tap meanwhile.
func ChownTapIn(namespace, name string, uid, gid int) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// The tun driver finds a tap by name in the namespace of the thread that opens /dev/net/tun.
		home, err := inNamespace(namespace, func() error { return chownTapNamed(name, uid, gid) })
		// A thread left in the namespace stays locked, so the runtime ends it with this goroutine.
		if home {
			runtime.UnlockOSThread()
		}
		done <- err
	}()

	return <-done
}

// inNamespace runs do on this locked thread inside the namespace, and reports whether the thread got back to its own.
func inNamespace(namespace string, do func() error) (bool, error) {
	return inNamespaceAt(NamespacePath(namespace), do)
}

// inNamespaceAt is inNamespace for the namespace file at path.
func inNamespaceAt(path string, do func() error) (bool, error) {
	self, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return true, fmt.Errorf("open this thread's network namespace: %w", err)
	}
	home, err := visit(self, path, do)

	return home, errors.Join(err, closeFD(self, "this thread's network namespace"))
}

// visit enters the namespace at path, runs do there and goes back to self.
func visit(self int, path string, do func() error) (bool, error) {
	target, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return true, fmt.Errorf("open %s: %w", path, err)
	}
	err = unix.Setns(target, unix.CLONE_NEWNET)
	closed := closeFD(target, path)
	if err != nil {
		return true, errors.Join(fmt.Errorf("enter %s: %w", path, err), closed)
	}
	err = errors.Join(do(), closed)
	if back := unix.Setns(self, unix.CLONE_NEWNET); back != nil {
		return false, errors.Join(err, fmt.Errorf("leave %s: %w", path, back))
	}

	return true, err
}

func chownTapNamed(name string, uid, gid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}

	return errors.Join(chownTap(fd, name, uid, gid), closeFD(fd, "/dev/net/tun"))
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

func closeFD(fd int, what string) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close %s: %w", what, err)
	}

	return nil
}
