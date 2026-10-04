//go:build linux

package runc_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

	_, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/true"}, Bundle: bundle(t), Launch: "/.shard/init"})
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

// A shim that died leaves its pid free for any host process, so a cancelled launch never signals what the pid file names.
func TestACancelledLaunchSparesTheProcessThePIDFileNames(t *testing.T) {
	bystander := exec.Command("sleep", "60")
	if err := bystander.Start(); err != nil {
		t.Fatalf("start the bystander: %v", err)
	}
	ended := make(chan error, 1)
	go func() { ended <- bystander.Wait() }()
	t.Cleanup(func() {
		if err := bystander.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the bystander: %v", err)
		}
	})

	r, argvFile := fakeBinary(t, writingPID(strconv.Itoa(bystander.Process.Pid))+`: > "$argv.ready"
exec sleep 30
`)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer cancel()

		waitFor(argvFile + ".ready")
	}()

	_, err := r.Exec(ctx, "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/sleep", "30"}, Bundle: bundle(t), Launch: "/.shard/init"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled Exec returned %v, want it to name the cancellation", err)
	}

	select {
	case err := <-ended:
		t.Fatalf("the process the pid file named ended with %v, want it untouched", err)
	case <-time.After(300 * time.Millisecond):
	}
}
