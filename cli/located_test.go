package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/presmihaylov/shard/services/client"
)

// A stranger reads where this host's daemon keeps its log, and an exit code survives the log line.
func TestWithLogNamesWhereTheLogIs(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"refusal", &client.APIError{Message: "the daemon log has the cause of the failure"}, "the daemon log has the cause of the failure (sudo journalctl -u shard)"},
		{"wrapped", fmt.Errorf("sandbox a1 failed to start: %s", "the sandbox stopped; the daemon log has the cause"), "sandbox a1 failed to start: the sandbox stopped; the daemon log has the cause (sudo journalctl -u shard)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withLog(c.err, "sudo journalctl -u shard").Error(); got != c.want {
				t.Errorf("withLog = %q, want %q", got, c.want)
			}
		})
	}

	err := withLog(&ExitError{Code: runFailedExitCode, Message: "the daemon could not complete the request; the daemon log has the cause"}, "sudo journalctl -u shard")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != runFailedExitCode || exit.Message != "the daemon could not complete the request; the daemon log has the cause (sudo journalctl -u shard)" {
		t.Errorf("withLog of an exit = %#v", err)
	}
}

// Another root and a remote keep the text as the daemon wrote it: setup serves the default root of this host only.
func TestLocatedLeavesAnotherRootAndARemoteAlone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := errors.New("the daemon log has the cause of the failure")
	for _, a := range []App{{Root: t.TempDir()}, {Root: DefaultRoot, Remote: "https://shard.example.com"}} {
		if err := a.located(in); err.Error() != in.Error() {
			t.Errorf("located(%+v) = %v", a, err)
		}
	}
}
