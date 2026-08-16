package bundle

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// errNoEntry lets a numeric id fall back to the plain id, while a real read error still propagates.
var errNoEntry = errors.New("no such entry in the image")

// A passwd line is name:x:uid:gid:...; a group line is name:x:gid:...
const (
	passwdFields = 4
	groupFields  = 3
)

// resolveUser turns an image USER into ids. A name is looked up in the image's own passwd and group.
func resolveUser(rootfs, user string) (specs.User, error) {
	if user == "" {
		return specs.User{}, nil
	}

	name, group, hasGroup := strings.Cut(user, ":")

	uid, gid, err := lookupUser(rootfs, name)
	if err != nil {
		return specs.User{}, err
	}

	if !hasGroup {
		return specs.User{UID: uid, GID: gid}, nil
	}

	gid, err = lookupGroup(rootfs, group)
	if err != nil {
		return specs.User{}, err
	}

	return specs.User{UID: uid, GID: gid}, nil
}

// lookupUser returns the primary gid too, which is what a USER with no group means.
func lookupUser(rootfs, name string) (uint32, uint32, error) {
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
			return wanted, 0, nil
		}

		return 0, 0, fmt.Errorf("resolve the user %q: %w", name, err)
	}

	uid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("the passwd entry for %q has an unreadable uid: %w", name, err)
	}

	gid, err := strconv.ParseUint(fields[3], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("the passwd entry for %q has an unreadable gid: %w", name, err)
	}

	return uint32(uid), uint32(gid), nil
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

func parseID(s string) (uint32, bool) {
	id, err := strconv.ParseUint(s, 10, 32)

	return uint32(id), err == nil
}

// findEntry reads one colon-separated database. A missing file is the same answer as a missing name.
func findEntry(path string, minFields int, match func(fields []string) bool) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the image has no %s: %w", filepath.Base(path), errNoEntry)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < minFields || !match(fields) {
			continue
		}

		return fields, nil
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return nil, errNoEntry
}
