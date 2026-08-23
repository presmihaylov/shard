package bundle_test

import (
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

	_, err := bundle.ResolveUser(rootfs, "root")
	if err == nil {
		t.Fatal("ResolveUser read a passwd file that points out of the rootfs")
	}
	if !strings.Contains(err.Error(), filepath.Join(rootfs, "etc/passwd")) {
		t.Errorf("the refusal is %q, and it must name the file", err)
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
		if !strings.Contains(err.Error(), "regular file") {
			t.Errorf("the refusal is %q, and it must say what the file must be", err)
		}
	case <-time.After(resolveBudget):
		t.Fatalf("ResolveUser did not answer within %s, so it is waiting on the fifo", resolveBudget)
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
	if !strings.Contains(err.Error(), filepath.Join(rootfs, "etc/group")) {
		t.Errorf("the refusal is %q, and it must name the file", err)
	}
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
