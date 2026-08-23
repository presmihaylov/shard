package bundle

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// errNoEntry lets a numeric id fall back to the plain id, while a real read error still propagates.
var errNoEntry = errors.New("no such entry in the image")

// A passwd line is name:x:uid:gid:...; a group line is name:x:gid:member,member.
const (
	passwdFields = 4
	groupFields  = 3
	memberField  = 3
)

// Identity is the whole of a user, because dropping to one means adopting all of it and not just its ids.
type Identity struct {
	UID uint32
	GID uint32
	// Groups is the supplementary set, the primary gid first, as initgroups(3) builds it for su and login.
	Groups []uint32
}

// ResolveUser turns a user into ids. A name is looked up in that rootfs's own passwd and group, so
// an exec resolves against the sandbox's live tree and a create against the image's.
// The caller asks only when someone named a user, so an empty one never reaches here.
func ResolveUser(rootfs, user string) (Identity, error) {
	name, group, hasGroup := strings.Cut(user, ":")

	entry, uid, gid, err := lookupUser(rootfs, name)
	if err != nil {
		return Identity{}, err
	}

	if hasGroup {
		gid, err = lookupGroup(rootfs, group)
		if err != nil {
			return Identity{}, err
		}
	}

	memberships, err := membershipsOf(rootfs, entry)
	if err != nil {
		return Identity{}, err
	}

	groups := []uint32{gid}
	for _, membership := range memberships {
		if !slices.Contains(groups, membership) {
			groups = append(groups, membership)
		}
	}

	return Identity{UID: uid, GID: gid, Groups: groups}, nil
}

// lookupUser returns the entry name and the primary gid too, which is what a USER with no group means.
func lookupUser(rootfs, name string) (string, uint32, uint32, error) {
	// A numeric USER resolves through passwd as well, so it gets that entry's primary group, as runc does.
	wanted, numeric := parseID(name)
	match := func(fields []string) bool { return fields[0] == name }
	if numeric {
		match = func(fields []string) bool { id, ok := parseID(fields[2]); return ok && id == wanted }
	}

	fields, err := findEntry(filepath.Join(rootfs, "etc/passwd"), passwdFields, match)
	if err != nil {
		// An id the image does not list is still a valid id to run as, and its group is root.
		if numeric && errors.Is(err, errNoEntry) {
			return "", wanted, 0, nil
		}

		return "", 0, 0, fmt.Errorf("resolve the user %q: %w", name, err)
	}

	uid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("the passwd entry for %q has an unreadable uid: %w", name, err)
	}

	gid, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("the passwd entry for %q has an unreadable gid: %w", name, err)
	}

	return fields[0], uint32(uid), uint32(gid), nil
}

// membershipsOf lists every group whose member list names the user. An image with no group file, or
// one that names the user nowhere, leaves it with its primary group alone, and neither fails a create.
func membershipsOf(rootfs, name string) ([]uint32, error) {
	// An id with no passwd entry has no name for a member list to hold, so it belongs to nothing else.
	if name == "" {
		return nil, nil
	}

	var groups []uint32
	err := scanDatabase(filepath.Join(rootfs, "etc/group"), memberField+1, func(fields []string) (bool, error) {
		if !slices.Contains(strings.Split(fields[memberField], ","), name) {
			return true, nil
		}

		gid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return false, fmt.Errorf("the group entry for %q has an unreadable gid: %w", fields[0], err)
		}

		groups = append(groups, uint32(gid))

		return true, nil
	})
	if errors.Is(err, errNoEntry) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve the groups of %q: %w", name, err)
	}

	return groups, nil
}

// lookupGroup needs no passwd trick: a numeric group carries everything a gid means.
func lookupGroup(rootfs, name string) (uint32, error) {
	if id, ok := parseID(name); ok {
		return id, nil
	}

	fields, err := findEntry(filepath.Join(rootfs, "etc/group"), groupFields, func(f []string) bool { return f[0] == name })
	if err != nil {
		return 0, fmt.Errorf("resolve the group %q: %w", name, err)
	}

	gid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("the group entry for %q has an unreadable gid: %w", name, err)
	}

	return uint32(gid), nil
}

// formatGroups spells a supplementary set for the -groups flag the supervisor is given.
func formatGroups(groups []uint32) string {
	fields := make([]string, len(groups))
	for i, gid := range groups {
		fields[i] = strconv.FormatUint(uint64(gid), 10)
	}

	return strings.Join(fields, ",")
}

// parseGroups reads a supplementary set back off the supervisor argv config.json recorded.
func parseGroups(groups string) ([]uint32, error) {
	if groups == "" {
		return nil, nil
	}

	var out []uint32
	for field := range strings.SplitSeq(groups, ",") {
		gid, ok := parseID(field)
		if !ok {
			return nil, fmt.Errorf("%q is not a gid", field)
		}

		out = append(out, gid)
	}

	return out, nil
}

func parseID(s string) (uint32, bool) {
	id, err := strconv.ParseUint(s, 10, 32)

	return uint32(id), err == nil
}

// findEntry returns the first entry the match accepts. A missing file is the same answer as a missing name.
func findEntry(path string, minFields int, match func(fields []string) bool) ([]string, error) {
	var found []string
	err := scanDatabase(path, minFields, func(fields []string) (bool, error) {
		if !match(fields) {
			return true, nil
		}
		found = fields

		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, errNoEntry
	}

	return found, nil
}

// scanDatabase reads one colon-separated database and stops when visit says it has read enough.
// The rootfs is the guest's own tree, so the file it names is refused unless it is a plain one: a
// symlink would resolve against the host's root, and a fifo would block this open until the guest answers.
func scanDatabase(path string, minFields int, visit func(fields []string) (bool, error)) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the image has no %s: %w", filepath.Base(path), errNoEntry)
	}
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("%s is a symbolic link, and a user database must be a file in the same tree", path)
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is a %s, and a user database must be a regular file", path, info.Mode().Type())
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < minFields {
			continue
		}

		more, err := visit(fields)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	return nil
}
