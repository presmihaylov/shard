//go:build linux

package runc_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/launch"
	"github.com/presmihaylov/shard/pkg/runc"
)

// A runc that fails before the shim runs, a bad cwd say, is no command that failed to start: its own log says why.
func TestALaunchRuncEndsBeforeTheShimKeepsItsLog(t *testing.T) {
	r, argvFile := fakeBinary(t, `while [ $# -gt 0 ]; do
	[ "$1" = --log ] && printf '%s\n' 'chdir to cwd ("/nope") set in config.json failed' > "$2"
	shift
done
exit 1
`)

	_, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/true"}, Launch: "/.shard/init"})
	if !errors.Is(err, launch.ErrNoShim) {
		t.Fatalf("Exec returned %v, want ErrNoShim", err)
	}
	if !strings.Contains(err.Error(), `chdir to cwd ("/nope")`) {
		t.Errorf("the error %q drops what runc logged", err)
	}

	// --log is a global flag, so runc reads it only ahead of the verb.
	got := argv(t, argvFile)
	if log, verb := slices.Index(got, "--log"), slices.Index(got, "exec"); log < 0 || log > verb {
		t.Errorf("the argv %q puts no --log ahead of exec", got)
	}
}
