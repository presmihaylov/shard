//go:build linux

package bundle

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Mount stacks the sandbox's writable layer over the shared image rootfs. It is safe to call twice.
func Mount(b Bundle) error {
	mounted, err := Mounted(b)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	// A colon or a comma in a layer path would be read as a separator, and overlayfs offers no escape.
	for _, dir := range []string{b.Lower, b.Upper, b.Work} {
		if strings.ContainsAny(dir, ":,") {
			return fmt.Errorf("the layer path %q contains a character overlayfs uses as a separator", dir)
		}
	}

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", b.Lower, b.Upper, b.Work)
	if err := syscall.Mount("overlay", b.RootFS, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount the overlay on %s: %w", b.RootFS, err)
	}

	return nil
}

// Unmount drops the merged view. The upper layer stays, which is what a stop and start relies on.
func Unmount(b Bundle) error {
	mounted, err := Mounted(b)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}

	// MNT_DETACH, because a leftover open file in the guest must not make a stop fail.
	if err := syscall.Unmount(b.RootFS, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", b.RootFS, err)
	}

	return nil
}

// Mounted asks the kernel rather than a record, because a shard restart forgets what it mounted.
func Mounted(b Bundle) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("open /proc/self/mountinfo: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Field 5 is the mount point, and the fields before it never contain a space.
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 5 && unescapeMountPoint(fields[4]) == b.RootFS {
			return true, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}

	return false, nil
}

// The kernel octal-escapes space, tab, newline and backslash in a mount point.
var mountPointEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescapeMountPoint(field string) string {
	return mountPointEscapes.Replace(field)
}
