//go:build linux

package netns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// addOwnedNamespace holds a child in the new pair just long enough to pin both namespaces to files,
// the way ip netns add pins a netns. The child is cat on a pipe: closing the pipe ends it, so no
// timer and no signal is involved.
func (m *Manager) addOwnedNamespace(ctx context.Context, name string, owner IDMapping) error {
	// A pin a crashed run left would hide the new namespace under it.
	if err := unpinUserns(name); err != nil {
		return err
	}

	child := exec.CommandContext(ctx, "cat")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: int(owner.HostID), Size: int(owner.Size)}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: int(owner.HostID), Size: int(owner.Size)}},
	}

	stdin, err := child.StdinPipe()
	if err != nil {
		return fmt.Errorf("namespace %s: pipe to the holder: %w", name, err)
	}

	if err := child.Start(); err != nil {
		return fmt.Errorf("namespace %s: hold a user namespace mapping 0 to %d for %d ids: %w", name, owner.HostID, owner.Size, err)
	}

	pinned := m.pin(ctx, name, child.Process.Pid)

	// The holder goes whether the pins landed or not; the pins keep the namespaces, not the process.
	return errors.Join(pinned, release(stdin, child))
}

// pin binds the holder's netns to the iproute2 name and its userns to the shard pin, so both outlive it.
func (m *Manager) pin(ctx context.Context, name string, pid int) error {
	if err := m.run(ctx, "netns", "attach", name, strconv.Itoa(pid)); err != nil {
		return err
	}

	target := UsernsPath(name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("make %s: %w", filepath.Dir(target), err)
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o444)
	if err != nil {
		return fmt.Errorf("make the pin %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close the pin %s: %w", target, err)
	}

	source := fmt.Sprintf("/proc/%d/ns/user", pid)
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("pin the user namespace of %d at %s: %w", pid, target, err)
	}

	return nil
}

// release ends the holder and reaps it.
func release(stdin io.Closer, child *exec.Cmd) error {
	if err := stdin.Close(); err != nil {
		return fmt.Errorf("release the namespace holder: %w", err)
	}

	if err := child.Wait(); err != nil {
		return fmt.Errorf("reap the namespace holder: %w", err)
	}

	return nil
}

// unpinUserns drops the user namespace pin, if there is one. A namespace nobody holds dies with its pin.
func unpinUserns(name string) error {
	target := UsernsPath(name)

	err := syscall.Unmount(target, 0)
	if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unpin the user namespace at %s: %w", target, err)
	}

	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the pin %s: %w", target, err)
	}

	return nil
}
