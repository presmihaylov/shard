//go:build integration

package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The entrypoint drops to the ids, and PID 1 keeps its own so the exit status still lands.
func TestTheEntrypointRunsAsTheGivenUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping to another user needs root")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	exitFile := filepath.Join(t.TempDir(), "exit.json")
	// A real /bin/sh, not this test binary: the go build cache is not readable by another user.
	cmd := exec.Command(exe, "-exit-file", exitFile, "-user", "65534:65534", "--", "/bin/sh", "-c", "id -u")
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)
	cmd.Stderr = os.Stderr

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe the supervisor stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}

	super := &supervisor{cmd: cmd, exitFile: exitFile, out: bufio.NewReader(pipe)}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}

		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Errorf("wait for the supervisor: %v", err)
		}
	})

	if got := super.line(t); got != "65534" {
		t.Errorf("the entrypoint reported uid %q, want 65534", got)
	}
	// The exit file is the whole point: a supervisor that dropped too could not write it.
	if status := super.awaitExitStatus(t); status.Code != 0 {
		t.Errorf("exit status is %+v, want code 0", status)
	}
}
