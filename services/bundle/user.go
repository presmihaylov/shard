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

	"github.com/presmihaylov/shard/pkg/filemode"
)

// errNoEntry lets a numeric id fall back to the plain id, while a real read error still propagates.
var errNoEntry = errors.New("no such entry in the image")

// UnknownUserError is a user or group the rootfs does not list: the request is wrong, not the sandbox.
type UnknownUserError struct {
	Err error
}

func (e *UnknownUserError) Error() string { return e.Err.Error() }

func (e *UnknownUserError) Unwrap() error { return e.Err }

func (e *UnknownUserError) Public() string { return e.Err.Error() }

// UserDatabaseError is a passwd or group the guest made something other than a regular file; it names the guest path, never the host's.
type UserDatabaseError struct {
	Err error
}

func (e *UserDatabaseError) Error() string { return e.Err.Error() }

func (e *UserDatabaseError) Public() string { return e.Err.Error() }

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

	fields, err := findEntry(rootfs, "etc/passwd", passwdFields, match)
	if err != nil {
		// An id the image does not list is still a valid id to run as, and its group is root.
		if numeric && errors.Is(err, errNoEntry) {
			return "", wanted, 0, nil
		}
		if errors.Is(err, errNoEntry) {
			return "", 0, 0, &UnknownUserError{Err: fmt.Errorf("resolve the user %q: %w", name, err)}
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
	err := scanDatabase(rootfs, "etc/group", memberField+1, func(fields []string) (bool, error) {
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

	fields, err := findEntry(rootfs, "etc/group", groupFields, func(f []string) bool { return f[0] == name })
	if errors.Is(err, errNoEntry) {
		return 0, &UnknownUserError{Err: fmt.Errorf("resolve the group %q: %w", name, err)}
	}
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
func findEntry(rootfs, rel string, minFields int, match func(fields []string) bool) ([]string, error) {
	var found []string
	err := scanDatabase(rootfs, rel, minFields, func(fields []string) (bool, error) {
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

// os.OpenRoot confines every part of rel to the guest's own tree, so no symlink on a middle part leads the read onto the host (SHARD-357).
func scanDatabase(rootfs, rel string, minFields int, visit func(fields []string) (bool, error)) error {
	full := filepath.Join(rootfs, rel)

	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return fmt.Errorf("open the rootfs %s: %w", rootfs, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	f, err := openDatabase(root, rel, full)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the image has no %s: %w", filepath.Base(rel), errNoEntry)
	}
	if _, refused := errors.AsType[*UserDatabaseError](err); refused {
		return err
	}
	if err != nil {
		return openFailed(root, rel, full, err)
	}
	defer f.Close()

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
		return fmt.Errorf("read %s: %w", full, err)
	}

	return nil
}

// requireDatabase refuses anything but a regular file, whose read can neither block nor reach a driver.
func requireDatabase(f *os.File, rel, full string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", full, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return linkRefused(rel)
	}
	if !info.Mode().IsRegular() {
		return notRegular(rel, info.Mode())
	}

	return nil
}

// openFailed names a link on the way, or a last part that is no regular file, as the guest's own, since a failed open says neither.
func openFailed(root *os.Root, rel, full string, err error) error {
	prefix := ""
	var mode fs.FileMode
	for part := range strings.SplitSeq(rel, "/") {
		prefix = filepath.Join(prefix, part)
		info, lstatErr := root.Lstat(prefix)
		if lstatErr != nil {
			return fmt.Errorf("open %s: %w", full, errors.Join(err, lstatErr))
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return linkRefused(prefix)
		}
		mode = info.Mode()
	}
	// A socket, or a device with no driver behind it, fails the open itself (ENXIO).
	if !mode.IsRegular() {
		return notRegular(rel, mode)
	}

	return fmt.Errorf("open %s: %w", full, err)
}

// linkRefused names a link the guest put on the way, whether a failed open or an O_PATH handle found it.
func linkRefused(part string) error {
	return &UserDatabaseError{Err: fmt.Errorf("/%s is a symbolic link, and a user database must be a file in the same tree", part)}
}

// maxLinks is the kernel's own bound on the links one path lookup follows.
const maxLinks = 40

// CheckUserDatabases refuses a passwd or group that is not a regular file, since runc opens both before every exec and a fifo stalls it (SHARD-653).
func CheckUserDatabases(rootfs string) error {
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return fmt.Errorf("open the rootfs %s: %w", rootfs, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	for _, rel := range []string{"etc/passwd", "etc/group"} {
		mode, err := guestMode(root, rel, nil)
		// A database the guest cannot reach either is one runc does without.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check the guest's /%s: %w", rel, err)
		}
		if !mode.IsRegular() {
			return notRegular(rel, mode)
		}
	}

	return nil
}

// errMounted is a path that reaches a guest mount, whose tree the host's view of the rootfs does not hold.
var errMounted = errors.New("the path reaches a mount of the guest")

// guestMode resolves links as the guest does, an absolute one from the top and ".." stopping there, and opens nothing a fifo could block.
func guestMode(root *os.Root, rel string, mounts map[string]string) (fs.FileMode, error) {
	parts := strings.Split(rel, "/")
	resolved := ""
	mode := fs.ModeDir
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if !mode.IsDir() {
			return 0, &fs.PathError{Op: "lstat", Path: resolved, Err: syscall.ENOTDIR}
		}
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = strings.TrimSuffix(filepath.Dir(resolved), ".")
			mode = fs.ModeDir
			continue
		}

		next := filepath.Join(resolved, part)
		source, mounted := mounts[next]
		if mounted && source == "" || !mounted && inMount(mounts, resolved) {
			return 0, &fs.PathError{Op: "lstat", Path: next, Err: errMounted}
		}
		// The runtime resolved a bind's source on the host, so a file bound there is a file to the guest.
		if mounted {
			info, err := os.Stat(source)
			if err != nil {
				return 0, err
			}
			resolved, mode = next, info.Mode()
			continue
		}
		info, err := root.Lstat(next)
		if err != nil {
			return 0, err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			resolved, mode = next, info.Mode()
			continue
		}

		links++
		if links > maxLinks {
			return 0, &fs.PathError{Op: "lstat", Path: next, Err: syscall.ELOOP}
		}
		target, err := root.Readlink(next)
		if err != nil {
			return 0, err
		}
		if filepath.IsAbs(target) {
			resolved = ""
		}
		parts = append(strings.Split(target, "/"), parts...)
	}

	return mode, nil
}

// inMount is a path at or under a mount, whose tree the rootfs does not hold.
func inMount(mounts map[string]string, rel string) bool {
	for dest := range mounts {
		if rel == dest || strings.HasPrefix(rel, dest+"/") {
			return true
		}
	}

	return false
}

func notRegular(rel string, mode fs.FileMode) error {
	return &UserDatabaseError{Err: fmt.Errorf("/%s is a %s, and a user database must be a regular file", rel, filemode.Name(mode))}
}
