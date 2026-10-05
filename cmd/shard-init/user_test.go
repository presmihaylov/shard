package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

func writeDatabases(t *testing.T, passwd, group string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}

	return root
}

// bob is what `useradd -u 2001 bob` leaves in a sandbox after it starts, which the image on the host never holds.
func TestLookupCredentialResolvesAgainstTheLiveTree(t *testing.T) {
	root := writeDatabases(t,
		"root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534::/:/sbin/nologin\nbob:x:2001:2001::/home/bob:/bin/sh\n",
		"root:x:0:\nwheel:x:10:bob\ndocker:x:999:root,bob\nbob:x:2001:\n",
	)

	cases := map[string]struct {
		user string
		want syscall.Credential
	}{
		"a user added after start": {user: "bob", want: syscall.Credential{Uid: 2001, Gid: 2001, Groups: []uint32{2001, 10, 999}}},
		"a name and a group name":  {user: "bob:wheel", want: syscall.Credential{Uid: 2001, Gid: 10, Groups: []uint32{10, 999}}},
		"a name and a gid":         {user: "bob:50", want: syscall.Credential{Uid: 2001, Gid: 50, Groups: []uint32{50, 10, 999}}},
		"a listed uid":             {user: "2001", want: syscall.Credential{Uid: 2001, Gid: 2001, Groups: []uint32{2001, 10, 999}}},
		"an unlisted uid":          {user: "3000", want: syscall.Credential{Uid: 3000, Gid: 0, Groups: []uint32{0}}},
		"an unlisted uid and gid":  {user: "3000:3000", want: syscall.Credential{Uid: 3000, Gid: 3000, Groups: []uint32{3000}}},
		"root":                     {user: "root", want: syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0, 999}}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := lookupCredential(root, c.user)
			if err != nil {
				t.Fatalf("lookupCredential(%q): %v", c.user, err)
			}
			if got.Uid != c.want.Uid || got.Gid != c.want.Gid || !slices.Equal(got.Groups, c.want.Groups) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// A name the sandbox does not hold is a start failure the CLI reports as 126, never as a missing binary's 127.
func TestLookupCredentialRefusesWhatTheSandboxDoesNotHold(t *testing.T) {
	root := writeDatabases(t, "bob:x:2001:2001::/home/bob:/bin/sh\nbad:x:nope:2001::/:/bin/sh\n", "bob:x:2001:\n")

	for name, user := range map[string]string{
		"an unknown user":   "alice",
		"an unknown group":  "bob:staff",
		"an unreadable uid": "bad",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := lookupCredential(root, user)
			if err == nil {
				t.Fatalf("lookupCredential(%q) returned no error", user)
			}
			if code := startFailureCode(err); code != 126 {
				t.Errorf("start failure code = %d, want 126: %v", code, err)
			}
		})
	}
}

func TestLookupCredentialWithoutDatabases(t *testing.T) {
	root := t.TempDir()

	got, err := lookupCredential(root, "1000")
	if err != nil {
		t.Fatalf("a uid with no passwd: %v", err)
	}
	if got.Uid != 1000 || got.Gid != 0 || !slices.Equal(got.Groups, []uint32{0}) {
		t.Errorf("got %+v, want uid 1000, gid 0", got)
	}
	if _, err := lookupCredential(root, "bob"); err == nil {
		t.Error("a name with no passwd returned no error")
	}
}

// A fifo at /etc/passwd would hang the exec on an open that waits for a writer.
func TestLookupCredentialRefusesADatabaseThatIsNotAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "etc/passwd"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := lookupCredential(root, "bob")
	if err == nil || !strings.Contains(err.Error(), "/etc/passwd is a named pipe") {
		t.Fatalf("a fifo at /etc/passwd gave %v, want it named a named pipe", err)
	}
}

// Without Lookup the guest takes the ids the host resolved, which is all an older host sends.
func TestExecCredentialTakesResolvedIDs(t *testing.T) {
	got, err := execCredential(supervisor.ExecHeader{User: "1000:1000", Groups: []uint32{1000, 10}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Uid != 1000 || got.Gid != 1000 || !slices.Equal(got.Groups, []uint32{1000, 10}) {
		t.Errorf("got %+v", got)
	}

	if _, err := execCredential(supervisor.ExecHeader{User: "bob"}); err == nil {
		t.Error("a name without Lookup returned no error")
	}
}
