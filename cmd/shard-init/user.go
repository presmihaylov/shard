package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/pkg/filemode"
	"github.com/presmihaylov/shard/services/supervisor"
)

// execCredential takes the ids the host resolved, or resolves the user the exec named against the live tree.
func execCredential(header supervisor.ExecHeader) (*syscall.Credential, error) {
	if header.Lookup {
		return lookupCredential("/", header.User)
	}

	return credentialOf(header.User, header.Groups)
}

// homeField is the passwd field that names a user's home.
const homeField = 5

// withHome sets the HOME runsc and runc take from the guest's passwd, which nothing else sets in a VM (SHARD-784).
func withHome(root string, env []string, credential *syscall.Credential) ([]string, error) {
	// A HOME already set wins even when empty, as runsc keeps it.
	if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "HOME=") }) {
		return env, nil
	}
	uid := uint32(0)
	if credential != nil {
		uid = credential.Uid
	}
	passwd, err := readDatabase(filepath.Join(root, "etc/passwd"), 3)
	if err != nil {
		return nil, err
	}

	return append(slices.Clip(env), "HOME="+homeOf(passwd, uid)), nil
}

// homeOf is what runsc finds: the home of the first entry with the uid, and "/" without one.
func homeOf(passwd [][]string, uid uint32) string {
	for _, fields := range passwd {
		if id, err := parseID(fields[2]); err != nil || id != uid {
			continue
		}
		// An entry with no home field leaves HOME empty, as runsc and runc both do.
		if len(fields) <= homeField {
			return ""
		}

		return fields[homeField]
	}

	return "/"
}

// lookupCredential follows the rules of bundle.ResolveUser, which the guest does not import: that package doubles its size.
func lookupCredential(root, user string) (*syscall.Credential, error) {
	name, group, hasGroup := strings.Cut(user, ":")

	passwd, err := readDatabase(filepath.Join(root, "etc/passwd"), 4)
	if err != nil {
		return nil, err
	}
	groups, err := readDatabase(filepath.Join(root, "etc/group"), 3)
	if err != nil {
		return nil, err
	}

	entry, uid, gid, err := userOf(passwd, name)
	if err != nil {
		return nil, err
	}
	if hasGroup {
		gid, err = groupOf(groups, group)
		if err != nil {
			return nil, err
		}
	}

	set := []uint32{gid}
	for _, fields := range groups {
		// An id with no passwd entry has no name for a member list to hold.
		if entry == "" || len(fields) < 4 || !slices.Contains(strings.Split(fields[3], ","), entry) {
			continue
		}
		member, err := parseID(fields[2])
		if err != nil {
			return nil, fmt.Errorf("the group entry for %q has an unreadable gid: %w", fields[0], err)
		}
		if !slices.Contains(set, member) {
			set = append(set, member)
		}
	}

	return &syscall.Credential{Uid: uid, Gid: gid, Groups: set}, nil
}

// userOf matches a number by uid and a name by name; a uid the sandbox does not list runs with group 0.
func userOf(passwd [][]string, name string) (string, uint32, uint32, error) {
	wanted, numErr := parseID(name)
	match := func(fields []string) bool { return fields[0] == name }
	if numErr == nil {
		match = func(fields []string) bool { id, err := parseID(fields[2]); return err == nil && id == wanted }
	}
	for _, fields := range passwd {
		if !match(fields) {
			continue
		}

		uid, err := parseID(fields[2])
		if err != nil {
			return "", 0, 0, fmt.Errorf("the passwd entry for %q has an unreadable uid: %w", name, err)
		}
		gid, err := parseID(fields[3])
		if err != nil {
			return "", 0, 0, fmt.Errorf("the passwd entry for %q has an unreadable gid: %w", name, err)
		}

		return fields[0], uid, gid, nil
	}
	if numErr == nil {
		return "", wanted, 0, nil
	}

	return "", 0, 0, fmt.Errorf("resolve the user %q: the sandbox's /etc/passwd has no such entry", name)
}

func groupOf(groups [][]string, name string) (uint32, error) {
	if id, err := parseID(name); err == nil {
		return id, nil
	}
	for _, fields := range groups {
		if fields[0] != name {
			continue
		}
		gid, err := parseID(fields[2])
		if err != nil {
			return 0, fmt.Errorf("the group entry for %q has an unreadable gid: %w", name, err)
		}

		return gid, nil
	}

	return 0, fmt.Errorf("resolve the group %q: the sandbox's /etc/group has no such entry", name)
}

// readDatabase reads one colon-separated database whole; a missing file holds no entries, and a fifo cannot block the exec.
func readDatabase(path string, minFields int) (entries [][]string, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", path, cerr))
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is a %s, and a user database must be a regular file", path, filemode.Name(info.Mode()))
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if fields := strings.Split(scanner.Text(), ":"); len(fields) >= minFields {
			entries = append(entries, fields)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return entries, nil
}
