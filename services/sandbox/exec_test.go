package sandbox_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// execOf is one exec over the fakes, with the streams a test reads back.
func execOf(t *testing.T, l layers, svc *sandbox.Service, ref string, req sandbox.ExecRequest, stdin string) (models.ExitStatus, *bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()

	var out, errOut bytes.Buffer

	streams := sandbox.Streams{Stdout: &out, Stderr: &errOut}
	if stdin != "" {
		streams.Stdin = strings.NewReader(stdin)
	}

	status, err := svc.Exec(t.Context(), ref, req, streams)

	return status, &out, &errOut, err
}

func TestExecRunsTheCommandAndReportsItsExitStatus(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execExit = models.ExitStatus{Code: 7}
	l.provider.execOut = "hello\n"
	l.provider.execErrOut = "careful\n"

	req := sandbox.ExecRequest{Command: []string{"sh", "-c", "exit 7"}, Env: []string{"A=1"}, WorkDir: "/srv", User: "app"}

	status, out, errOut, err := execOf(t, l, svc, "sandbox1", req, "typed\n")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if status.Code != 7 {
		t.Errorf("exit code = %d, want 7", status.Code)
	}
	if out.String() != "hello\n" || errOut.String() != "careful\n" {
		t.Errorf("stdout = %q, stderr = %q", out, errOut)
	}
	if l.provider.execInput != "typed\n" {
		t.Errorf("the command read %q, want typed", l.provider.execInput)
	}
	if want := []string{"sh", "-c", "exit 7"}; !slices.Equal(l.provider.execSpec.Argv, want) {
		t.Errorf("argv = %v, want %v", l.provider.execSpec.Argv, want)
	}
	if l.provider.execSpec.WorkDir != "/srv" || l.provider.execSpec.User != "app" {
		t.Errorf("workDir = %q, user = %q, want /srv and app", l.provider.execSpec.WorkDir, l.provider.execSpec.User)
	}
}

// Without stdin the command reads nothing, and the substrate must be given no descriptor for it.
func TestExecWithoutStdinGivesTheCommandNone(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	if _, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if l.provider.execSpec.Stdin != nil {
		t.Error("the command was given stdin, and nobody asked for it")
	}
}

func TestExecRefusesARequestWithNoCommand(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{}, "")
	if err == nil || !strings.Contains(err.Error(), "no command") {
		t.Fatalf("Exec of nothing returned %v, want a refusal", err)
	}
	if slices.Contains(r.calls, "provider.Exec") {
		t.Error("exec reached the provider with no command to run")
	}
}

// The keep-alive rule: the entrypoint exits, the sandbox stays running, and exec still works on it.
func TestExecRunsInASandboxWhoseEntrypointHasExited(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.status = models.Status{Exists: true, State: models.StateRunning}

	if _, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
}

func TestExecRefusesASandboxTheRecordDoesNotHold(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.missing = true

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, "")
	if !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("Exec returned %v, want a sandbox that is not found", err)
	}
	if slices.Contains(r.calls, "provider.Exec") {
		t.Error("exec reached the provider for a sandbox shard does not hold")
	}
}

// A record that says stopped outranks the oom count the cgroup kept: the user stopped this one.
func TestExecRefusesAStoppedSandboxWithoutTheProvider(t *testing.T) {
	r := &recorder{}
	sb := running()
	sb.State = models.StateStopped
	svc, l := newService(t, r, sb)
	l.provider.status = models.Status{Exists: true, State: models.StateStopped, OOMKilled: true}

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, "")
	if err == nil || !strings.Contains(err.Error(), "shard start sandbox1") {
		t.Fatalf("Exec of a stopped sandbox returned %v, want the start hint", err)
	}
	if strings.Contains(err.Error(), "memory") {
		t.Errorf("the refusal is %q, and the user stopped this sandbox", err)
	}
	if slices.Contains(r.calls, "provider.Exec") {
		t.Error("exec reached the provider for a stopped sandbox")
	}
}

// The provider holds nothing of a paused sandbox, so the refusal points at resume and not at gone.
func TestExecRefusesAPausedSandboxWithTheResumeHint(t *testing.T) {
	r := &recorder{}
	sb := running()
	sb.State = models.StatePaused
	svc, l := newService(t, r, sb)
	l.provider.status = models.Status{}

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, "")
	if err == nil || !strings.Contains(err.Error(), "shard resume sandbox1") {
		t.Fatalf("Exec of a paused sandbox returned %v, want the resume hint", err)
	}
}

// A record outlives a host restart, so the substrate is the one that answers for the state.
func TestExecRefusesASandboxTheProviderNoLongerHolds(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.status = models.Status{}

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, "")
	if err == nil || !strings.Contains(err.Error(), "gone from fake") {
		t.Fatalf("Exec returned %v, want a sandbox the substrate no longer holds", err)
	}
}

// The exit file records a 137 for an oom kill and for a plain kill -9, so the reason is named here.
func TestExecNamesTheMemoryTheSandboxRanOutOf(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.status = models.Status{OOMKilled: true}

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, "")
	if err == nil || !strings.Contains(err.Error(), "ran out of memory") {
		t.Fatalf("Exec returned %v, want the memory named", err)
	}
	if !strings.Contains(err.Error(), "--memory") {
		t.Errorf("the refusal is %q, and it must say what to do about it", err)
	}
}

// A command the substrate refused to start is the caller's to turn into a shell's exit code.
func TestExecReportsACommandThatNeverRan(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execErr = &models.CommandNotStartedError{Sandbox: "sandbox1", Reason: "failed to load /bin/nope: no such file or directory", Code: 127}

	_, _, _, err := execOf(t, l, svc, "sandbox1", sandbox.ExecRequest{Command: []string{"/bin/nope"}}, "")

	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) {
		t.Fatalf("Exec returned %v, want a command that never ran", err)
	}
	if notStarted.Code != 127 {
		t.Errorf("code = %d, want 127", notStarted.Code)
	}
}

// The exec id names the session for as long as it runs, and a resize reaches the pty by it.
func TestExecNamesTheSessionBeforeItRuns(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	var named string
	streams := sandbox.Streams{Started: func(execID string) error {
		named = execID

		return nil
	}}

	if _, err := svc.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, streams); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if named == "" {
		t.Fatal("the exec was never named")
	}
	if l.provider.execID != "sandbox1" {
		t.Errorf("the provider was given id %q, want sandbox1", l.provider.execID)
	}
}

// A client that goes away before the command runs ends the exec, and the substrate is never reached.
func TestExecEndsWhenTheClientCannotBeAnswered(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	streams := sandbox.Streams{Started: func(string) error { return errors.New("the client is gone") }}

	if _, err := svc.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, streams); err == nil {
		t.Fatal("Exec ran a command nobody was left to answer")
	}
	if slices.Contains(r.calls, "provider.Exec") {
		t.Error("exec reached the provider for a client that had gone")
	}
}

// A resize of an exec that has ended is a resize of nothing, and it says so rather than pretending.
func TestResizeExecRefusesAnExecNobodyIsRunning(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	err := svc.ResizeExec(t.Context(), "sandbox1", "1a2b3c4d5e6f7a8b", sandbox.TerminalSize{Rows: 24, Cols: 80})
	if !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("ResizeExec returned %v, want an exec that is not found", err)
	}
}
