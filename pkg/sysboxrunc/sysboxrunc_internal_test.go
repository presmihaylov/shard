package sysboxrunc

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
