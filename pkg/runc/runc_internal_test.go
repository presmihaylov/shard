package runc

import (
	"slices"
	"testing"
)

// A tty exec needs a real pty on stdin to fork at all, so the flag is checked on the argv builder alone.
func TestExecArgsAsksForATTY(t *testing.T) {
	args := execArgs("amber-otter-1a2b", "/tmp/pid", ExecOptions{Argv: []string{"/bin/sh"}, TTY: true})

	if !slices.Equal(args, []string{"exec", "--pid-file", "/tmp/pid", "--tty", "amber-otter-1a2b", "/bin/sh"}) {
		t.Errorf("got argv %q, want --tty before the id and nothing else", args)
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
