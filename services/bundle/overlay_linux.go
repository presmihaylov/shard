//go:build linux

package bundle

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
)

// Mount stacks the sandbox's writable layer over the shared image rootfs. It is safe to call twice.
func (b Bundle) Mount(lower string) error {
	mounted, err := b.Mounted()
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, b.Upper, b.Work)
	if err := syscall.Mount("overlay", b.RootFS, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount the overlay on %s: %w", b.RootFS, err)
	}

	return nil
}

// Unmount drops the merged view. The upper layer stays, which is what a stop and start relies on.
func (b Bundle) Unmount() error {
	mounted, err := b.Mounted()
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
func (b Bundle) Mounted() (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("open /proc/self/mountinfo: %w", err)
	}
	defer f.Close()

	// A later line shadows an earlier one at the same point, so the last match is the effective mount.
	fsType, superOptions, found := "", "", false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Field 5 is the mount point, and the fields before it never contain a space.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || unescapeMountInfo(fields[4]) != b.RootFS {
			continue
		}

		// The optional fields are variable in number, so a lone "-" is what separates them from the rest.
		sep := indexSeparator(fields)
		if sep < 0 || len(fields) < sep+4 {
			return false, fmt.Errorf("/proc/self/mountinfo has an unreadable line for %s", b.RootFS)
		}

		fsType, superOptions, found = fields[sep+1], unescapeMountInfo(fields[sep+3]), true
	}

	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	if !found {
		return false, nil
	}

	// Refuse rather than run on a mount we did not make, or detach one we do not own.
	if fsType != "overlay" || !slices.Contains(strings.Split(superOptions, ","), "upperdir="+b.Upper) {
		return false, fmt.Errorf("%s already holds a %s mount that is not this sandbox's overlay", b.RootFS, fsType)
	}

	return true, nil
}

// The optional fields start at index 6, so an earlier field that happens to be "-" is not the separator.
func indexSeparator(fields []string) int {
	const optionalFieldsStart = 6

	if len(fields) < optionalFieldsStart {
		return -1
	}

	sep := slices.Index(fields[optionalFieldsStart:], "-")
	if sep < 0 {
		return -1
	}

	return optionalFieldsStart + sep
}

// The kernel octal-escapes space, tab, newline and backslash in a path.
var mountInfoEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescapeMountInfo(field string) string {
	return mountInfoEscapes.Replace(field)
}
