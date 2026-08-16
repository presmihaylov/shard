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
	if id, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(id), 0, nil
	}

	fields, err := findEntry(filepath.Join(rootfs, "etc/passwd"), name, 4)
	if err != nil {
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

func lookupGroup(rootfs, name string) (uint32, error) {
	if id, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(id), nil
	}

	fields, err := findEntry(filepath.Join(rootfs, "etc/group"), name, 3)
	if err != nil {
		return 0, fmt.Errorf("resolve the group %q: %w", name, err)
	}

	gid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("the group entry for %q has an unreadable gid: %w", name, err)
	}

	return uint32(gid), nil
}

// findEntry reads one colon-separated database. A missing file is the same answer as a missing name.
func findEntry(path, name string, minFields int) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the image has no %s", filepath.Base(path))
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < minFields || fields[0] != name {
			continue
		}

		return fields, nil
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return nil, errors.New("no such entry in the image")
}
