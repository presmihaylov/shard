package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// execOf is one exec over the fakes, created and then attached, with the streams a test reads back.
func execOf(t *testing.T, _ layers, svc *sandbox.Service, ref string, req sandbox.ExecRequest, stdin string) (models.ExitStatus, *bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()

	var out, errOut bytes.Buffer

	streams := sandbox.Streams{Stdout: &out, Stderr: &errOut}
	if stdin != "" {
		streams.Stdin = strings.NewReader(stdin)
		req.Stdin = true
	}

	exec, err := svc.CreateExec(t.Context(), ref, req)
	if err != nil {
		return models.ExitStatus{}, &out, &errOut, err
	}

	status, err := svc.Attach(t.Context(), ref, exec.ID, streams)

	return status, &out, &errOut, err
}

// attach is the second step alone, for a test that holds the exec id itself.
func attach(t *testing.T, svc *sandbox.Service, ref, execID string, streams sandbox.Streams) (models.ExitStatus, error) {
	t.Helper()

	return svc.Attach(t.Context(), ref, execID, streams)
}

// notifyWriter closes wrote the first time it is written to, so a test can wait for the replay to land.
type notifyWriter struct {
	buf   bytes.Buffer
	once  sync.Once
	wrote chan struct{}
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.wrote) })

	return w.buf.Write(p)
}

// The command starts the moment the create returns, whether or not a client ever attaches.
func TestCreateExecStartsTheCommandAndNamesIt(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	if exec.ID == "" {
		t.Fatal("the exec was never named")
	}
	if exec.Sandbox != "sandbox1" {
		t.Errorf("the exec names sandbox %q, want sandbox1", exec.Sandbox)
	}
	if exec.State != models.ExecRunning {
		t.Errorf("the exec is %q, want running", exec.State)
	}

	got, err := svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec: %v", err)
	}
	if got.State != models.ExecRunning {
		t.Errorf("the exec is %q with no client attached, want running", got.State)
	}

	close(l.provider.execWaits)
}

// The command runs and can be waited on with nobody ever attaching to it.
func TestAnExecRunsWithNoClientAttached(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})
	l.provider.execExit = models.ExitStatus{Code: 5}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	close(l.provider.execWaits)

	done, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("WaitExec: %v", err)
	}
	if done.State != models.ExecExited {
		t.Errorf("the exec is %q, want exited", done.State)
	}
	if done.ExitStatus == nil || done.ExitStatus.Code != 5 {
		t.Errorf("the exec ended with %+v, want code 5", done.ExitStatus)
	}
}

func TestAttachRefusesAnExecNobodyCreated(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	_, err := attach(t, svc, "sandbox1", "1a2b3c4d5e6f7a8b", sandbox.Streams{})
	if !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("Attach returned %v, want an exec that is not found", err)
	}
	if slices.Contains(r.calls, "provider.Exec") {
		t.Error("the attach reached the provider for an exec nobody created")
	}
}

// One attach per exec: the second finds it taken. After the command ends, an attach replays it instead.
func TestAttachRefusesASecondAttach(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	attached := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		streams := sandbox.Streams{Stdout: io.Discard, Started: func(string) error {
			close(attached)

			return nil
		}}
		_, err := attach(t, svc, "sandbox1", exec.ID, streams)
		first <- err
	}()

	<-attached

	_, err = attach(t, svc, "sandbox1", exec.ID, sandbox.Streams{})

	var inUse *sandbox.AttachedError
	if !errors.As(err, &inUse) || inUse.ID != exec.ID {
		t.Errorf("the second attach returned %v, want the exec named as attached", err)
	}

	close(l.provider.execWaits)

	if err := <-first; err != nil {
		t.Errorf("the first attach returned %v", err)
	}

	// The command is over, but the record stays, so an attach now replays it and answers the exit.
	if _, err := attach(t, svc, "sandbox1", exec.ID, sandbox.Streams{}); err != nil {
		t.Errorf("an attach after the end returned %v, want a replay and the exit", err)
	}
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

// The attach names the exec to the client before it replays anything, so a resize reaches the pty by it.
func TestAttachNamesTheExecBeforeTheReplay(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	var named string
	streams := sandbox.Streams{Started: func(execID string) error {
		named = execID

		return nil
	}}

	if _, err := attach(t, svc, "sandbox1", exec.ID, streams); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if named != exec.ID {
		t.Fatalf("the exec was named %q, want %s", named, exec.ID)
	}
	if l.provider.execID != "sandbox1" {
		t.Errorf("the provider was given id %q, want sandbox1", l.provider.execID)
	}
}

// A client that cannot be answered ends its own attach, and the command it left runs on regardless.
func TestAttachEndsWhenTheClientCannotBeAnswered(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})
	l.provider.execExit = models.ExitStatus{Code: 9}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	streams := sandbox.Streams{Started: func(string) error { return errors.New("the client is gone") }}
	if _, err := attach(t, svc, "sandbox1", exec.ID, streams); err == nil {
		t.Fatal("the attach answered a client that had gone")
	}

	got, err := svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec: %v", err)
	}
	if got.State != models.ExecRunning {
		t.Errorf("the command is %q after the client left, want running", got.State)
	}

	close(l.provider.execWaits)

	done, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("WaitExec: %v", err)
	}
	if done.ExitStatus == nil || done.ExitStatus.Code != 9 {
		t.Errorf("the command ended with %+v, want code 9", done.ExitStatus)
	}
}

// A client that drops leaves the command running, and the next attach replays the output and the exit.
func TestAttachReplaysAfterADropAndReturnsTheExit(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})
	l.provider.execOut = "live\n"
	l.provider.execExit = models.ExitStatus{Code: 4}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	out1 := &notifyWriter{wrote: make(chan struct{})}
	ctx1, cancel1 := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := svc.Attach(ctx1, "sandbox1", exec.ID, sandbox.Streams{Stdout: out1})
		first <- err
	}()

	<-out1.wrote
	cancel1()

	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the dropped attach returned %v, want a cancelled context", err)
	}

	got, err := svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec: %v", err)
	}
	if got.State != models.ExecRunning {
		t.Errorf("the command is %q after the client dropped, want running", got.State)
	}

	var out2 bytes.Buffer
	second := make(chan error, 1)
	status := make(chan models.ExitStatus, 1)
	go func() {
		exit, err := svc.Attach(t.Context(), "sandbox1", exec.ID, sandbox.Streams{Stdout: &out2})
		status <- exit
		second <- err
	}()

	close(l.provider.execWaits)

	if err := <-second; err != nil {
		t.Fatalf("the re-attach returned %v", err)
	}
	if exit := <-status; exit.Code != 4 {
		t.Errorf("the re-attach answered code %d, want 4", exit.Code)
	}
	if out2.String() != "live\n" {
		t.Errorf("the re-attach replayed %q, want live", out2.String())
	}
}

func TestGetExecAnswersTheRecordThenTheExit(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})
	l.provider.execExit = models.ExitStatus{Code: 2}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	got, err := svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec: %v", err)
	}
	if got.State != models.ExecRunning || got.ExitStatus != nil {
		t.Errorf("the running exec is %+v, want running with no exit", got)
	}

	close(l.provider.execWaits)

	if _, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID); err != nil {
		t.Fatalf("WaitExec: %v", err)
	}

	got, err = svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec after the end: %v", err)
	}
	if got.State != models.ExecExited || got.ExitStatus == nil || got.ExitStatus.Code != 2 {
		t.Errorf("the ended exec is %+v, want exited with code 2", got)
	}
}

func TestGetExecRefusesAnExecNobodyCreated(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	if _, err := svc.GetExec(t.Context(), "sandbox1", "1a2b3c4d5e6f7a8b"); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("GetExec returned %v, want an exec that is not found", err)
	}
}

func TestListExecsAnswersEverySandboxExec(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	first, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}
	second, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	execs, err := svc.ListExecs(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("ListExecs: %v", err)
	}
	if len(execs) != 2 {
		t.Fatalf("the list holds %d execs, want 2", len(execs))
	}
	if !slices.IsSortedFunc(execs, func(a, b models.Exec) int { return strings.Compare(a.ID, b.ID) }) {
		t.Errorf("the list is not sorted by id: %v", execs)
	}

	ids := []string{execs[0].ID, execs[1].ID}
	if !slices.Contains(ids, first.ID) || !slices.Contains(ids, second.ID) {
		t.Errorf("the list %v is missing one of %s and %s", ids, first.ID, second.ID)
	}
}

func TestListExecsRefusesASandboxTheRecordDoesNotHold(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.missing = true

	if _, err := svc.ListExecs(t.Context(), "sandbox1"); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("ListExecs returned %v, want a sandbox that is not found", err)
	}
}

// A kill waits for the pid the provider reported, then signals it, and the default signal is TERM.
func TestKillExecSignalsTheRunningCommand(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.signaled = make(chan struct{})
	l.provider.execPID = 4242
	l.provider.execExit = models.ExitStatus{Code: 143}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	if err := svc.KillExec(t.Context(), "sandbox1", exec.ID, ""); err != nil {
		t.Fatalf("KillExec: %v", err)
	}

	done, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("WaitExec: %v", err)
	}
	if done.ExitStatus == nil || done.ExitStatus.Code != 143 {
		t.Errorf("the killed command ended with %+v, want code 143", done.ExitStatus)
	}
	if l.provider.signalGot != "TERM" {
		t.Errorf("the provider was sent %q, want TERM", l.provider.signalGot)
	}
	if l.provider.signalPID != 4242 {
		t.Errorf("the provider signalled pid %d, want 4242", l.provider.signalPID)
	}
}

func TestKillExecRefusesAnExecThatHasExited(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	if _, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID); err != nil {
		t.Fatalf("WaitExec: %v", err)
	}

	err = svc.KillExec(t.Context(), "sandbox1", exec.ID, "TERM")

	var exited *sandbox.ExecExitedError
	if !errors.As(err, &exited) || exited.ID != exec.ID {
		t.Errorf("the kill of an ended exec returned %v, want the exec named as exited", err)
	}
}

func TestKillExecRefusesAnUnknownSignal(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	err = svc.KillExec(t.Context(), "sandbox1", exec.ID, "HUP")
	if err == nil || !strings.Contains(err.Error(), "TERM or KILL") {
		t.Fatalf("the kill with an unknown signal returned %v, want a refusal", err)
	}
	if slices.Contains(r.calls, "provider.Signal") {
		t.Error("an unknown signal still reached the provider")
	}

	close(l.provider.execWaits)
}

func TestDeleteExecRefusesAnExecStillRunning(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	err = svc.DeleteExec(t.Context(), "sandbox1", exec.ID)

	var runningErr *sandbox.ExecRunningError
	if !errors.As(err, &runningErr) || runningErr.ID != exec.ID {
		t.Errorf("the delete of a running exec returned %v, want the exec named as running", err)
	}

	close(l.provider.execWaits)
}

func TestDeleteExecForgetsAnExecThatHasEnded(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	if _, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID); err != nil {
		t.Fatalf("WaitExec: %v", err)
	}

	if err := svc.DeleteExec(t.Context(), "sandbox1", exec.ID); err != nil {
		t.Fatalf("DeleteExec: %v", err)
	}

	if _, err := svc.GetExec(t.Context(), "sandbox1", exec.ID); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("a get after the delete returned %v, want an exec that is not found", err)
	}
}

// The daemon keeps only the last execBufferCap bytes, and a replay that lost bytes is marked truncated.
func TestExecKeepsTheLastBytesAndMarksItTruncated(t *testing.T) {
	const cap = 8 << 20

	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execOut = strings.Repeat("a", cap+(1<<20))

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	// The command ends first, so the buffer holds only its evicted tail and an attach replays that.
	if _, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID); err != nil {
		t.Fatalf("WaitExec: %v", err)
	}

	var out bytes.Buffer
	if _, err := svc.Attach(t.Context(), "sandbox1", exec.ID, sandbox.Streams{Stdout: &out}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if out.Len() > cap {
		t.Errorf("the replay is %d bytes, want no more than the %d byte cap", out.Len(), cap)
	}
	if out.Len() <= cap-(1<<20) {
		t.Errorf("the replay is %d bytes, want the buffer kept close to its %d byte cap", out.Len(), cap)
	}

	got, err := svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec: %v", err)
	}
	if !got.Truncated {
		t.Error("the record is not marked truncated, and the replay lost its oldest bytes")
	}
}

// A command with stdin reads nothing until a client attaches and types, then ends the input.
func TestExecStdinReachesTheCommandOnlyThroughAnAttach(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execExit = models.ExitStatus{Code: 5}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	got, err := svc.GetExec(t.Context(), "sandbox1", exec.ID)
	if err != nil {
		t.Fatalf("GetExec: %v", err)
	}
	if got.State != models.ExecRunning {
		t.Errorf("the command is %q with no client attached, want running on a stdin it cannot read yet", got.State)
	}

	streams := sandbox.Streams{Stdin: strings.NewReader("hi\n"), Stdout: io.Discard}
	status, err := svc.Attach(t.Context(), "sandbox1", exec.ID, streams)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if status.Code != 5 {
		t.Errorf("the command ended with code %d, want 5", status.Code)
	}
	if l.provider.execInput != "hi\n" {
		t.Errorf("the command read %q, want hi", l.provider.execInput)
	}
}

// A resize of an exec nobody created, and of one that runs on pipes, is a resize of nothing. A real
// terminal needs Linux, so the resize of a running terminal is proven in the integration suite.
func TestResizeExecRefusesAnExecWithNoTerminal(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})

	if err := svc.ResizeExec(t.Context(), "sandbox1", "1a2b3c4d5e6f7a8b", sandbox.TerminalSize{Rows: 24, Cols: 80}); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("ResizeExec of an unknown exec returned %v, want an exec that is not found", err)
	}

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	if err := svc.ResizeExec(t.Context(), "sandbox1", exec.ID, sandbox.TerminalSize{Rows: 24, Cols: 80}); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("ResizeExec of a pipe exec returned %v, want an exec with no terminal", err)
	}

	close(l.provider.execWaits)
}

// A stop takes the sandbox's execs with it, so a get after the stop finds nothing.
func TestStopForgetsTheSandboxExecs(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.provider.execWaits = make(chan struct{})

	exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}})
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := svc.GetExec(t.Context(), "sandbox1", exec.ID); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("a get after the stop returned %v, want an exec that is not found", err)
	}
}

// A sandbox that runs many execs does not grow without a bound, even when no later create runs. The cap
// runs when each exec exits, so retention settles at the cap on its own and the oldest answers not-found.
func TestManyExecsEvictTheOldestNotTheNewest(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	const runs = 100
	ids := make([]string, 0, runs)
	for i := range runs {
		exec, err := svc.CreateExec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}})
		if err != nil {
			t.Fatalf("CreateExec %d: %v", i, err)
		}
		if _, err := svc.WaitExec(t.Context(), "sandbox1", exec.ID); err != nil {
			t.Fatalf("WaitExec %d: %v", i, err)
		}
		ids = append(ids, exec.ID)
	}

	// The cap runs in each exec's own goroutine as it exits, so retention settles a moment after the waits.
	held := waitForExecCount(t, svc, "sandbox1", sandbox.ExitedExecCap)
	if held != sandbox.ExitedExecCap {
		t.Errorf("the sandbox holds %d of the %d runs, want the cap %d", held, runs, sandbox.ExitedExecCap)
	}

	// The oldest run is evicted and answers not-found, the newest is kept: eviction drops the oldest.
	if _, err := svc.GetExec(t.Context(), "sandbox1", ids[0]); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("the oldest exec answered %v, want not found after eviction", err)
	}
	if _, err := svc.GetExec(t.Context(), "sandbox1", ids[runs-1]); err != nil {
		t.Errorf("the newest exec answered %v, want it kept", err)
	}
}

// waitForExecCount polls until the sandbox holds want execs, or the deadline passes, and answers the last count.
func waitForExecCount(t *testing.T, svc *sandbox.Service, ref string, want int) int {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		list, err := svc.ListExecs(t.Context(), ref)
		if err != nil {
			t.Fatalf("ListExecs: %v", err)
		}
		if len(list) == want || time.Now().After(deadline) {
			return len(list)
		}

		time.Sleep(time.Millisecond)
	}
}

// A failed sandbox accepts only a delete of the sandbox itself, so every exec verb answers
// sandbox_failed. The guard runs before the exec lookup, so a made-up exec id still gets refused.
func TestExecVerbsRefuseAFailedSandbox(t *testing.T) {
	sb := running()
	sb.State = models.StateFailed
	sb.FailedReason = "the image pull failed"
	svc, _ := newService(t, &recorder{}, sb)

	var size sandbox.TerminalSize
	var streams sandbox.Streams
	verbs := map[string]func() error{
		"GetExec":    func() error { _, err := svc.GetExec(t.Context(), "sandbox1", "exec1"); return err },
		"WaitExec":   func() error { _, err := svc.WaitExec(t.Context(), "sandbox1", "exec1"); return err },
		"ListExecs":  func() error { _, err := svc.ListExecs(t.Context(), "sandbox1"); return err },
		"KillExec":   func() error { return svc.KillExec(t.Context(), "sandbox1", "exec1", "TERM") },
		"DeleteExec": func() error { return svc.DeleteExec(t.Context(), "sandbox1", "exec1") },
		"ResizeExec": func() error { return svc.ResizeExec(t.Context(), "sandbox1", "exec1", size) },
		"Attach":     func() error { _, err := svc.Attach(t.Context(), "sandbox1", "exec1", streams); return err },
	}

	for name, call := range verbs {
		var refused *sandbox.StateError
		if err := call(); !errors.As(err, &refused) || refused.Code != models.CodeSandboxFailed {
			t.Errorf("%s of a failed sandbox = %v, want %s", name, err, models.CodeSandboxFailed)
		}
	}
}
