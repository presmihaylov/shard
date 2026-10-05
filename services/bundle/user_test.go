package bundle_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/bundle"
)

// resolveBudget bounds a lookup that must answer, so a database that blocks fails rather than hangs.
const resolveBudget = 5 * time.Second

// An exec resolves against the sandbox's live tree, and a guest with root in it writes what it likes.
func TestResolveUserRefusesAPasswdThatIsASymbolicLink(t *testing.T) {
	rootfs := emptyRootFS(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(rootfs, "etc/passwd")); err != nil {
		t.Fatalf("link the passwd file: %v", err)
	}

	// A numeric id falls back only when passwd lists nobody, never past a file the daemon will not read.
	for _, user := range []string{"root", "65534"} {
		_, err := bundle.ResolveUser(rootfs, user)
		if err == nil {
			t.Fatalf("ResolveUser(%q) read a passwd file that points out of the rootfs", user)
		}
		requireGuestRefusal(t, err, rootfs, "/etc/passwd is a symbolic link")
	}
}

// A user or group the rootfs does not list is the caller's mistake, which a files verb answers 400 for (SHARD-426).
func TestResolveUserTypesANameTheRootFSDoesNotList(t *testing.T) {
	rootfs := rootFSWith(t, "build:x:1000:1000:build:/home/build:/bin/sh\n", "build:x:1000:\n")
	for user, named := range map[string]string{"nosuch": `user "nosuch"`, "build:nogroup": `group "nogroup"`} {
		_, err := bundle.ResolveUser(rootfs, user)
		if !errors.As(err, new(*bundle.UnknownUserError)) || !strings.Contains(err.Error(), named) {
			t.Errorf("ResolveUser(%q) = %v, want an unknown user error that names the %s", user, err, named)
		}
	}
	if _, err := bundle.ResolveUser(emptyRootFS(t), "nosuch"); !errors.As(err, new(*bundle.UnknownUserError)) {
		t.Errorf("ResolveUser on a rootfs with no passwd = %v, want an unknown user error", err)
	}
	for _, user := range []string{"build", "build:1000", "4242"} {
		if _, err := bundle.ResolveUser(rootfs, user); err != nil {
			t.Errorf("ResolveUser(%q) = %v, want it resolved", user, err)
		}
	}
}

// A fifo answers only when the guest writes to it, so an unbounded open is a hang the operator cannot end.
func TestResolveUserRefusesAPasswdThatIsAFifo(t *testing.T) {
	rootfs := emptyRootFS(t)
	if err := syscall.Mkfifo(filepath.Join(rootfs, "etc/passwd"), 0o600); err != nil {
		t.Fatalf("make the passwd fifo: %v", err)
	}

	failed := make(chan error, 1)
	go func() {
		_, err := bundle.ResolveUser(rootfs, "root")
		failed <- err
	}()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("ResolveUser read a passwd file that is a fifo")
		}
		requireGuestRefusal(t, err, rootfs, "/etc/passwd is a named pipe")
		if !strings.Contains(err.Error(), "regular file") {
			t.Errorf("the refusal is %q, and it must say what the file must be", err)
		}
	case <-time.After(resolveBudget):
		t.Fatalf("ResolveUser did not answer within %s, so it is waiting on the fifo", resolveBudget)
	}
}

// runc opens both databases before every exec, whatever the user, so the check refuses any non-file the guest's own lookup reaches (SHARD-653).
func TestCheckUserDatabasesRefusesWhatTheGuestLookupReaches(t *testing.T) {
	cases := map[string]struct {
		lay  func(t *testing.T, rootfs string)
		want string
	}{
		"a fifo passwd": {func(t *testing.T, rootfs string) { mkfifo(t, rootfs, "etc/passwd") }, "/etc/passwd is a named pipe"},
		"a fifo group":  {func(t *testing.T, rootfs string) { mkfifo(t, rootfs, "etc/group") }, "/etc/group is a named pipe"},
		"a directory group": {func(t *testing.T, rootfs string) {
			if err := os.Mkdir(filepath.Join(rootfs, "etc/group"), 0o755); err != nil {
				t.Fatalf("make the group directory: %v", err)
			}
		}, "/etc/group is a directory"},
		"an absolute link to a fifo": {func(t *testing.T, rootfs string) {
			mkfifo(t, rootfs, "fifo")
			symlink(t, "/fifo", filepath.Join(rootfs, "etc/passwd"))
		}, "/etc/passwd is a named pipe"},
		"a relative link to a fifo": {func(t *testing.T, rootfs string) {
			mkfifo(t, rootfs, "fifo")
			symlink(t, "../fifo", filepath.Join(rootfs, "etc/group"))
		}, "/etc/group is a named pipe"},
		"a link that climbs past the top": {func(t *testing.T, rootfs string) {
			mkfifo(t, rootfs, "fifo")
			symlink(t, "../../../../fifo", filepath.Join(rootfs, "etc/passwd"))
		}, "/etc/passwd is a named pipe"},
		"a linked etc": {func(t *testing.T, rootfs string) {
			if err := os.Mkdir(filepath.Join(rootfs, "real"), 0o755); err != nil {
				t.Fatalf("make the real etc: %v", err)
			}
			mkfifo(t, rootfs, "real/passwd")
			if err := os.Remove(filepath.Join(rootfs, "etc")); err != nil {
				t.Fatalf("drop the etc dir: %v", err)
			}
			symlink(t, "/real", filepath.Join(rootfs, "etc"))
		}, "/etc/passwd is a named pipe"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rootfs := emptyRootFS(t)
			c.lay(t, rootfs)

			requireGuestRefusal(t, bundle.CheckUserDatabases(rootfs), rootfs, c.want)
		})
	}
}

// A database runc cannot reach either is one it does without, and a link never leads the check onto the host.
func TestCheckUserDatabasesPassesWhatRuncReadsOrDoesWithout(t *testing.T) {
	host := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(host, 0o600); err != nil {
		t.Fatalf("make the host fifo: %v", err)
	}
	cases := map[string]func(t *testing.T) string{
		"regular files": func(t *testing.T) string { return rootFSWith(t, "root:x:0:0:::\n", "root:x:0:\n") },
		"no files":      emptyRootFS,
		"a link to a regular file": func(t *testing.T) string {
			rootfs := rootFSWith(t, "root:x:0:0:::\n", "")
			symlink(t, "/etc/passwd", filepath.Join(rootfs, "etc/group"))
			return rootfs
		},
		"a dangling link": func(t *testing.T) string {
			rootfs := emptyRootFS(t)
			symlink(t, "/nowhere", filepath.Join(rootfs, "etc/passwd"))
			return rootfs
		},
		"a link loop": func(t *testing.T) string {
			rootfs := emptyRootFS(t)
			symlink(t, "passwd", filepath.Join(rootfs, "etc/passwd"))
			return rootfs
		},
		"an etc that is a file": func(t *testing.T) string {
			rootfs := emptyRootFS(t)
			if err := os.Remove(filepath.Join(rootfs, "etc")); err != nil {
				t.Fatalf("drop the etc dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(rootfs, "etc"), nil, 0o600); err != nil {
				t.Fatalf("write etc as a file: %v", err)
			}
			return rootfs
		},
		"a link to a host fifo": func(t *testing.T) string {
			rootfs := emptyRootFS(t)
			symlink(t, host, filepath.Join(rootfs, "etc/passwd"))
			return rootfs
		},
	}
	for name, lay := range cases {
		t.Run(name, func(t *testing.T) {
			if err := bundle.CheckUserDatabases(lay(t)); err != nil {
				t.Errorf("CheckUserDatabases = %v, want nil", err)
			}
		})
	}
}

func mkfifo(t *testing.T, rootfs, rel string) {
	t.Helper()

	if err := syscall.Mkfifo(filepath.Join(rootfs, rel), 0o600); err != nil {
		t.Fatalf("make the fifo %s: %v", rel, err)
	}
}

func emptyRootFS(t *testing.T) string {
	t.Helper()

	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("create the rootfs: %v", err)
	}

	return rootfs
}

// Dropping to a user means adopting its whole identity, and its secondary groups are part of that.
func TestResolveUserAdoptsTheSecondaryGroups(t *testing.T) {
	rootfs := rootFSWith(t,
		"root:x:0:0:root:/root:/bin/sh\nbuild:x:1000:1000:build:/home/build:/bin/sh\n",
		"root:x:0:\nbuild:x:1000:\nwheel:x:10:build,root\ndocker:x:999:build\nother:x:20:someone\n")

	cases := map[string]struct {
		user string
		want bundle.Identity
	}{
		// The primary gid leads the set, as initgroups(3) builds it for su and for login.
		"a name":            {user: "build", want: bundle.Identity{UID: 1000, GID: 1000, Groups: []uint32{1000, 10, 999}}},
		"a numeric user":    {user: "1000", want: bundle.Identity{UID: 1000, GID: 1000, Groups: []uint32{1000, 10, 999}}},
		"a named group":     {user: "build:wheel", want: bundle.Identity{UID: 1000, GID: 10, Groups: []uint32{10, 999}}},
		"a group in no set": {user: "build:other", want: bundle.Identity{UID: 1000, GID: 20, Groups: []uint32{20, 10, 999}}},
		// root is in wheel by its member list, so a user in nothing else still gets what it is named in.
		"root": {user: "root", want: bundle.Identity{UID: 0, GID: 0, Groups: []uint32{0, 10}}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := bundle.ResolveUser(rootfs, c.user)
			if err != nil {
				t.Fatalf("ResolveUser(%q): %v", c.user, err)
			}
			if got.UID != c.want.UID || got.GID != c.want.GID || !slices.Equal(got.Groups, c.want.Groups) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// None of these may fail a create: an image is free to say nothing about the user it is run as.
func TestResolveUserFallsBackToThePrimaryGroupAlone(t *testing.T) {
	const passwd = "root:x:0:0:root:/root:/bin/sh\nbuild:x:1000:1000:build:/home/build:/bin/sh\n"

	cases := map[string]struct {
		passwd string
		group  string
		user   string
		want   bundle.Identity
	}{
		"no group file":           {passwd: passwd, user: "build", want: bundle.Identity{UID: 1000, GID: 1000, Groups: []uint32{1000}}},
		"in no member list":       {passwd: passwd, group: "build:x:1000:\n", user: "build", want: bundle.Identity{UID: 1000, GID: 1000, Groups: []uint32{1000}}},
		"a group with no members": {passwd: passwd, group: "build:x:1000\n", user: "build", want: bundle.Identity{UID: 1000, GID: 1000, Groups: []uint32{1000}}},
		// A raw uid the image does not list has no name, so no member list can name it either.
		"an unlisted uid":       {passwd: passwd, group: "wheel:x:10:build\n", user: "4242", want: bundle.Identity{UID: 4242, GID: 0, Groups: []uint32{0}}},
		"an unlisted pair":      {passwd: passwd, group: "wheel:x:10:build\n", user: "4242:4343", want: bundle.Identity{UID: 4242, GID: 4343, Groups: []uint32{4343}}},
		"no passwd file at all": {group: "wheel:x:10:build\n", user: "4242:4343", want: bundle.Identity{UID: 4242, GID: 4343, Groups: []uint32{4343}}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := bundle.ResolveUser(rootFSWith(t, c.passwd, c.group), c.user)
			if err != nil {
				t.Fatalf("ResolveUser(%q): %v", c.user, err)
			}
			if got.UID != c.want.UID || got.GID != c.want.GID || !slices.Equal(got.Groups, c.want.Groups) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// The group file is read for every user now, so the hardening the passwd file got has to cover it too.
func TestResolveUserRefusesAGroupFileThatIsASymbolicLink(t *testing.T) {
	rootfs := rootFSWith(t, "build:x:1000:1000:build:/home/build:/bin/sh\n", "")
	if err := os.Symlink("/etc/group", filepath.Join(rootfs, "etc/group")); err != nil {
		t.Fatalf("link the group file: %v", err)
	}

	_, err := bundle.ResolveUser(rootfs, "build")
	if err == nil {
		t.Fatal("ResolveUser read a group file that points out of the rootfs")
	}
	requireGuestRefusal(t, err, rootfs, "/etc/group is a symbolic link")
}

// A directory is no more a database than a fifo is, and the guest can make one as easily.
func TestResolveUserRefusesAPasswdThatIsADirectory(t *testing.T) {
	rootfs := emptyRootFS(t)
	if err := os.Mkdir(filepath.Join(rootfs, "etc/passwd"), 0o755); err != nil {
		t.Fatalf("make the passwd directory: %v", err)
	}

	_, err := bundle.ResolveUser(rootfs, "root")
	if err == nil {
		t.Fatal("ResolveUser read a passwd that is a directory")
	}
	requireGuestRefusal(t, err, rootfs, "/etc/passwd is a directory")
}

// A socket, like a device with no driver, fails the open itself, so the file type check after it never runs.
func TestResolveUserRefusesAPasswdThatIsASocket(t *testing.T) {
	rootfs, err := os.MkdirTemp("", "u") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("create the rootfs: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(rootfs); err != nil {
			t.Errorf("remove the rootfs: %v", err)
		}
	})
	if err := os.Mkdir(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("create etc: %v", err)
	}
	listener, err := net.Listen("unix", filepath.Join(rootfs, "etc/passwd"))
	if err != nil {
		t.Fatalf("make the passwd socket: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close the passwd socket: %v", err)
		}
	})

	_, err = bundle.ResolveUser(rootfs, "root")
	if err == nil {
		t.Fatal("ResolveUser read a passwd that is a socket")
	}
	requireGuestRefusal(t, err, rootfs, "/etc/passwd is a socket")
}

// requireGuestRefusal holds a database refusal to what a public route may answer: the guest's path, never where the host keeps the tree (SHARD-648).
func requireGuestRefusal(t *testing.T, err error, rootfs, want string) {
	t.Helper()

	refused, ok := errors.AsType[*bundle.UserDatabaseError](err)
	if !ok {
		t.Fatalf("the refusal %q is not a user database error, so the API answers it 500", err)
	}
	if !strings.HasPrefix(refused.Public(), want) || strings.Contains(refused.Public(), rootfs) {
		t.Errorf("the refusal reads %q, want it to start %q and never name the host rootfs %s", refused.Public(), want, rootfs)
	}
}

// O_NOFOLLOW guards only the last part, so a guest that makes a middle part a symlink onto the host could
// turn a host file into the user database; os.OpenRoot has to confine every part to the rootfs (SHARD-357).
func TestResolveUserRefusesAPasswdReachedThroughAHostSymlink(t *testing.T) {
	rootfs := emptyRootFS(t)
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "passwd"), []byte("probe:x:4242:4242:::\n"), 0o600); err != nil {
		t.Fatalf("write the host passwd: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(rootfs, "etc")); err != nil {
		t.Fatalf("drop the etc dir: %v", err)
	}
	if err := os.Symlink(host, filepath.Join(rootfs, "etc")); err != nil {
		t.Fatalf("link etc onto the host: %v", err)
	}

	got, err := bundle.ResolveUser(rootfs, "probe")
	if err == nil {
		t.Fatalf("ResolveUser read a passwd reached through a host symlink and returned %+v", got)
	}
	requireGuestRefusal(t, err, rootfs, "/etc is a symbolic link")
}

// rootFSWith writes the two databases. An empty one is a rootfs that has no such file at all.
func rootFSWith(t *testing.T, passwd, group string) string {
	t.Helper()

	rootfs := emptyRootFS(t)
	for name, content := range map[string]string{"passwd": passwd, "group": group} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(rootfs, "etc", name), []byte(content), 0o600); err != nil {
			t.Fatalf("write the %s file: %v", name, err)
		}
	}

	return rootfs
}
