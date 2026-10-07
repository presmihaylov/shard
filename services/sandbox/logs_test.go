package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// logsOf gives sb the process web and writes what it had written into the log the provider names for it.
func logsOf(t *testing.T, r *recorder, sb models.Sandbox, written string) (*sandbox.Service, layers, string) {
	t.Helper()

	sb.Processes = append(sb.Processes, proc("web", models.RestartNo))
	svc, l := newService(t, r, sb)

	path := filepath.Join(l.provider.logDir, "web.log")
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	return svc, l, path
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("append to the log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the log: %v", err)
	}
}

func TestLogsPrintsWhatTheProcessWrote(t *testing.T) {
	var out bytes.Buffer

	svc, _, _ := logsOf(t, &recorder{}, running(), "hello\nworld\n")

	if err := svc.Logs(t.Context(), "sandbox1", "web", &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out.String() != "hello\nworld\n" {
		t.Errorf("Logs printed %q", out.String())
	}
}

// The log outlives the run: a stopped sandbox still answers with everything its process wrote.
func TestLogsReadsAStoppedSandbox(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped
	svc, _, _ := logsOf(t, &recorder{}, sb, "done\n")

	if err := svc.Logs(t.Context(), "sandbox1", "web", &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out.String() != "done\n" {
		t.Errorf("Logs printed %q", out.String())
	}
}

func TestLogsTakesASandboxName(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.Name = "site"
	svc, _, _ := logsOf(t, &recorder{}, sb, "named\n")

	if err := svc.Logs(t.Context(), "site", "web", &out); err != nil {
		t.Fatalf("Logs of a name: %v", err)
	}
	if out.String() != "named\n" {
		t.Errorf("Logs printed %q", out.String())
	}
}

func TestLogsRefusesAnIDThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	svc, l, _ := logsOf(t, &recorder{}, running(), "")
	l.repo.missing = true

	err := svc.Logs(t.Context(), "sandbox1", "web", &out)
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("Logs returned %v, want the id named", err)
	}
}

// A name the sandbox never ran has no log, and a pending sandbox has run none.
func TestLogsRefusesAProcessTheSandboxNeverRan(t *testing.T) {
	for _, sb := range []models.Sandbox{running(), {ID: "sandbox1", State: models.StatePending}} {
		var out bytes.Buffer
		svc, _ := newService(t, &recorder{}, sb)

		err := svc.Logs(t.Context(), "sandbox1", "web", &out)
		if refused, ok := errors.AsType[*sandbox.StateError](err); !ok || refused.Code != models.CodeNoProcess {
			t.Errorf("Logs of a %s sandbox returned %v, want no_process", sb.State, err)
		}
		_, err = svc.FollowLogs(t.Context(), "sandbox1", "web", &out)
		if refused, ok := errors.AsType[*sandbox.StateError](err); !ok || refused.Code != models.CodeNoProcess {
			t.Errorf("FollowLogs of a %s sandbox returned %v, want no_process", sb.State, err)
		}
	}
}

// A follow drains what arrives after it started, and ends on its own once the sandbox has stopped.
func TestFollowDrainsThenEndsWhenTheSandboxStops(t *testing.T) {
	var out bytes.Buffer

	svc, l, path := logsOf(t, &recorder{}, running(), "first\n")
	l.provider.exits = func() { appendTo(t, path, "second\n") }

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", "web", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if out.String() != "first\nsecond\n" {
		t.Errorf("FollowLogs printed %q, want both lines", out.String())
	}
	if reason != sandbox.LogsStopped {
		t.Errorf("the follow ended with %q, want %s", reason, sandbox.LogsStopped)
	}
}

// A process that ended under a sandbox that runs on ends the follow, after its last line.
func TestFollowEndsWhenTheProcessEnds(t *testing.T) {
	var out bytes.Buffer

	svc, l, path := logsOf(t, &recorder{}, running(), "first\n")
	reads := 0
	l.provider.onTable = func() {
		reads++
		if reads == 2 {
			appendTo(t, path, "last\n")
			l.provider.table = []models.ProcessReport{{Name: "web", ProcessStatus: models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{}}}}
		}
	}

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", "web", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if out.String() != "first\nlast\n" || reason != sandbox.LogsEnded {
		t.Errorf("FollowLogs printed %q and ended with %q, want both lines and %s", out.String(), reason, sandbox.LogsEnded)
	}
}

// A sandbox removed under the follow ends it too, and the reason tells the client which it was.
func TestFollowSaysWhenTheSandboxWasRemoved(t *testing.T) {
	var out bytes.Buffer

	svc, l, _ := logsOf(t, &recorder{}, running(), "gone\n")
	l.provider.exits = func() { l.repo.missing = true }

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", "web", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if reason != sandbox.LogsRemoved {
		t.Errorf("the follow ended with %q, want %s", reason, sandbox.LogsRemoved)
	}
}

// An interrupt is how an operator leaves a follow on a process that still runs, and it is no error.
func TestFollowLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	svc, _, _ := logsOf(t, &recorder{}, running(), "up\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	reason, err := svc.FollowLogs(ctx, "sandbox1", "web", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if out.String() != "up\n" || reason != "" {
		t.Errorf("FollowLogs printed %q and ended with %q, want the line and no reason", out.String(), reason)
	}
}

func TestFollowReportsASubstrateItCannotAsk(t *testing.T) {
	var out bytes.Buffer

	svc, _, _ := logsOf(t, &recorder{fail: []string{"provider.Status"}}, running(), "")

	if _, err := svc.FollowLogs(t.Context(), "sandbox1", "web", &out); err == nil {
		t.Fatal("FollowLogs returned no error for a substrate it could not ask")
	}
}

// A start that fails under the follow fails the record, and the follow ends with what a new logs call answers.
func TestFollowEndsWithTheRefusalWhenTheSandboxFailsUnderIt(t *testing.T) {
	var out bytes.Buffer

	svc, l, _ := logsOf(t, &recorder{}, running(), "")
	l.provider.exits = func() {
		l.repo.sb.State = models.StateFailed
		l.repo.sb.FailedPublic = `resolve the user "nobody2": no such entry in the image`
	}

	_, err := svc.FollowLogs(t.Context(), "sandbox1", "web", &out)
	refused, ok := errors.AsType[*sandbox.StateError](err)
	if !ok || refused.Code != models.CodeSandboxFailed || !strings.Contains(refused.Public(), "nobody2") {
		t.Errorf("the follow returned %v, want sandbox_failed with the create's reason", err)
	}
}
