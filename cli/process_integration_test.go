//go:build integration

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// run --attach of a process that never starts again prints its output and leaves with its code, and the sandbox stays up.
func TestRunAttachExitsWithTheCodeOfTheProcess(t *testing.T) {
	app, out := newCreateApp(t)

	id := printedID(t, app, out, createArgs(testImage))
	t.Cleanup(func() { cleanUp(t, app, id) })

	attached, _ := ownStderr(app)
	err := attached.Run(t.Context(), []string{"run", id, "--name", processName, "--restart", "no", "--attach", "--", "/bin/sh", "-c", "echo hello; exit 3"})
	exit, ok := errors.AsType[*ExitError](err)
	if !ok || exit.Code != 3 {
		t.Fatalf("run --attach failed with %v, want the exit code 3 of the process", err)
	}
	if out.String() != "hello\n" {
		t.Errorf("run --attach printed %q, want the output of the process alone", out.String())
	}

	if sb := record(t, app, id); sb.State != models.StateRunning {
		t.Errorf("the record says %q after the process exited, want running", sb.State)
	}
}

// kill ends one process and cancels its restarts, and the sandbox and its other processes stay up.
func TestKillEndsOneProcessAndKeepsTheSandbox(t *testing.T) {
	app, out := newCreateApp(t)

	id := runDetachedWith(t, app, out, nil, []string{"--restart", "always"}, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	if err := app.Run(t.Context(), []string{"run", id, "--name", "other", "--", "/bin/sleep", "600"}); err != nil {
		t.Fatalf("run other: %v", err)
	}
	out.Reset()

	if err := app.Run(t.Context(), []string{"kill", id, processName}); err != nil {
		t.Fatalf("kill: %v", err)
	}

	procs, err := daemonClient(app).Processes(t.Context(), id)
	if err != nil {
		t.Fatalf("list the processes: %v", err)
	}
	states := map[string]models.ProcessState{}
	for _, p := range procs {
		states[p.Name] = p.Status.State
	}
	if states[processName] != models.ProcessKilled || states["other"] != models.ProcessRunning {
		t.Errorf("the processes are %v, want %s killed and other running", states, processName)
	}
	if sb := record(t, app, id); sb.State != models.StateRunning {
		t.Errorf("the record says %q after the kill, want running", sb.State)
	}
}

// A start brings back the always process a stop ended, under the name it had, and its log keeps the run before.
func TestAStartBringsBackAnAlwaysProcess(t *testing.T) {
	app, out := newCreateApp(t)

	id := runDetachedWith(t, app, out, nil, []string{"--restart", "always"}, "/bin/sh", "-c", "echo up; exec sleep 600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := app.Run(t.Context(), []string{"start", id}); err != nil {
		t.Fatalf("start: %v", err)
	}
	out.Reset()

	p, err := daemonClient(app).Process(t.Context(), id, processName)
	if err != nil {
		t.Fatalf("read the process: %v", err)
	}
	if p.Status.State != models.ProcessRunning {
		t.Errorf("the process is %s after the start, want running", p.Status.State)
	}

	if err := app.Run(t.Context(), []string{"logs", id, processName}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(out.String(), "up\n") {
		t.Errorf("logs printed %q, want the output of the run before the stop", out.String())
	}
}
