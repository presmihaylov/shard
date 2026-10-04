package runc

import (
	"slices"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// runc passes --additional-gids only beside --user, so an exec with no user keeps the bundle's whole user.
func TestExecProcessKeepsTheBundleUserWhenTheExecNamesNone(t *testing.T) {
	base := specs.Process{User: specs.User{UID: 1000, GID: 1000, AdditionalGids: []uint32{4}}, Cwd: "/home"}

	process, err := execProcess(base, ExecOptions{Argv: []string{"/bin/true"}, Groups: []uint32{10}})
	if err != nil {
		t.Fatalf("execProcess: %v", err)
	}
	if process.User.UID != 1000 || process.User.GID != 1000 || !slices.Equal(process.User.AdditionalGids, []uint32{4}) || process.Cwd != "/home" {
		t.Errorf("got %+v in %s, want the bundle's user and cwd", process.User, process.Cwd)
	}
}

// runc's --user 65534 changes the uid alone, so the bundle's gid stays.
func TestExecProcessSetsTheGIDOnlyWhenTheUserNamesOne(t *testing.T) {
	base := specs.Process{User: specs.User{UID: 1000, GID: 1000}}

	process, err := execProcess(base, ExecOptions{Argv: []string{"/bin/true"}, User: "65534"})
	if err != nil {
		t.Fatalf("execProcess: %v", err)
	}
	if process.User.UID != 65534 || process.User.GID != 1000 {
		t.Errorf("got %+v, want uid 65534 and the bundle's gid", process.User)
	}

	if _, err := execProcess(base, ExecOptions{Argv: []string{"/bin/true"}, User: "nobody"}); err == nil {
		t.Error("execProcess took a user name, which only the guest's passwd can resolve")
	}
}

// Without the flag runc joins a fresh session keyring per container, one key of the uid's quota each (SHARD-367).
func TestCreateArgsSkipTheKeyringOnlyWhenAsked(t *testing.T) {
	if args := createArgs("amber-otter-1a2b", "/b", true); !slices.Equal(args, []string{"create", "--bundle", "/b", "--no-new-keyring", "amber-otter-1a2b"}) {
		t.Errorf("got argv %q, want --no-new-keyring before the id", args)
	}
	if args := createArgs("amber-otter-1a2b", "/b", false); !slices.Equal(args, []string{"create", "--bundle", "/b", "amber-otter-1a2b"}) {
		t.Errorf("got argv %q, want no keyring flag", args)
	}
}
