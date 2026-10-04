package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// logsOf wires the output file the provider names and writes what the entrypoint had written into it.
func logsOf(t *testing.T, r *recorder, sb models.Sandbox, written string) (*sandbox.Service, layers, string) {
	t.Helper()

	svc, l := newService(t, r, sb)

	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatalf("write the output file: %v", err)
	}
	l.provider.logPath = path

	return svc, l, path
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open the output file: %v", err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("append to the output file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the output file: %v", err)
	}
}

func TestLogsPrintsWhatTheEntrypointWrote(t *testing.T) {
	var out bytes.Buffer

	svc, _, _ := logsOf(t, &recorder{}, running(), "hello\nworld\n")

	if err := svc.Logs(t.Context(), "sandbox1", &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out.String() != "hello\nworld\n" {
		t.Errorf("Logs printed %q", out.String())
	}
}

// The output outlives the entrypoint: a stopped sandbox still answers with everything it wrote.
func TestLogsReadsAStoppedSandbox(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped
	svc, _, _ := logsOf(t, &recorder{}, sb, "done\n")

	if err := svc.Logs(t.Context(), "sandbox1", &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if out.String() != "done\n" {
		t.Errorf("Logs printed %q", out.String())
	}
}

func TestLogsTakesAName(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	sb := running()
	sb.Name = "web"
	svc, _, _ := logsOf(t, r, sb, "named\n")

	if err := svc.Logs(t.Context(), "web", &out); err != nil {
		t.Fatalf("Logs of a name: %v", err)
	}
	if out.String() != "named\n" {
		t.Errorf("Logs printed %q", out.String())
	}
	if !slices.Contains(r.calls, "provider.LogPath") {
		t.Errorf("Logs never asked the provider for the path: %v", r.calls)
	}
}

func TestLogsRefusesAnIDThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	svc, l, _ := logsOf(t, &recorder{}, running(), "")
	l.repo.missing = true

	err := svc.Logs(t.Context(), "sandbox1", &out)
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("Logs returned %v, want the id named", err)
	}
}

// A follow drains what arrives after it started, and ends on its own once the sandbox has stopped.
func TestFollowDrainsThenEndsWhenTheSandboxStops(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	svc, l, path := logsOf(t, r, running(), "first\n")
	l.provider.exits = func() { appendTo(t, path, "second\n") }

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", &out)
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

// A sandbox removed under the follow ends it too, and the reason tells the client which it was.
func TestFollowSaysWhenTheSandboxWasRemoved(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	svc, l, _ := logsOf(t, r, running(), "gone\n")
	l.provider.exits = func() { l.repo.missing = true }

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if reason != sandbox.LogsRemoved {
		t.Errorf("the follow ended with %q, want %s", reason, sandbox.LogsRemoved)
	}
}

// An interrupt is how an operator leaves a follow on a sandbox that is still up, and it is no error.
func TestFollowLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	svc, _, _ := logsOf(t, &recorder{}, running(), "up\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	reason, err := svc.FollowLogs(ctx, "sandbox1", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if out.String() != "up\n" || reason != "" {
		t.Errorf("FollowLogs printed %q and ended with %q, want the line and no reason", out.String(), reason)
	}
}

func TestFollowReportsASubstrateItCannotAsk(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Status"}}
	svc, _, _ := logsOf(t, r, running(), "")

	if _, err := svc.FollowLogs(t.Context(), "sandbox1", &out); err == nil {
		t.Fatal("FollowLogs returned no error for a substrate it could not ask")
	}
}

func pending() models.Sandbox {
	return models.Sandbox{ID: "sandbox1", State: models.StatePending}
}

// settles moves the record to what the create ended in at the first poll after the follow began.
func settles(l layers, end func(*models.Sandbox)) {
	gets := 0
	l.repo.onGet = func() {
		gets++
		if gets == 2 {
			end(&l.repo.sb)
		}
	}
}

func failedCreate(sb *models.Sandbox) {
	sb.State = models.StateFailed
	sb.FailedPublic = `resolve the user "nobody2": no such entry in the image`
}

func refusedAsFailed(t *testing.T, err error) {
	t.Helper()

	refused, ok := errors.AsType[*sandbox.StateError](err)
	if !ok || refused.Code != models.CodeSandboxFailed || !strings.Contains(refused.Public(), "nobody2") {
		t.Errorf("the follow returned %v, want sandbox_failed with the create's reason", err)
	}
}

// A create still pulling has no output yet, so logs answers what a created sandbox that never ran does.
func TestLogsOfAPendingSandboxAnswersEmpty(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	svc, _ := newService(t, r, pending())

	if err := svc.Logs(t.Context(), "sandbox1", &out); err != nil {
		t.Fatalf("Logs of a pending sandbox: %v", err)
	}
	if out.Len() != 0 || slices.Contains(r.calls, "provider.LogPath") {
		t.Errorf("Logs printed %q after %v, want nothing and no output file asked for", out.String(), r.calls)
	}
}

func TestFollowOfAPendingSandboxFollowsOnceItRuns(t *testing.T) {
	var out bytes.Buffer

	svc, l, _ := logsOf(t, &recorder{}, pending(), "up\n")
	settles(l, func(sb *models.Sandbox) { sb.State = models.StateRunning })
	l.provider.exits = func() {}

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", &out)
	if err != nil {
		t.Fatalf("FollowLogs: %v", err)
	}
	if out.String() != "up\n" || reason != sandbox.LogsStopped {
		t.Errorf("FollowLogs printed %q and ended with %q, want the line and %s", out.String(), reason, sandbox.LogsStopped)
	}
}

func TestFollowOfAPendingSandboxEndsWhenTheCreateFails(t *testing.T) {
	var out bytes.Buffer

	svc, l := newService(t, &recorder{}, pending())
	settles(l, failedCreate)

	_, err := svc.FollowLogs(t.Context(), "sandbox1", &out)
	refusedAsFailed(t, err)
}

func TestFollowOfAPendingSandboxSaysWhenItWasRemoved(t *testing.T) {
	var out bytes.Buffer

	svc, l := newService(t, &recorder{}, pending())
	settles(l, func(*models.Sandbox) { l.repo.missing = true })

	reason, err := svc.FollowLogs(t.Context(), "sandbox1", &out)
	if err != nil || reason != sandbox.LogsRemoved {
		t.Errorf("FollowLogs ended with %q, %v, want %s", reason, err, sandbox.LogsRemoved)
	}
}

func TestFollowOfAPendingSandboxLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	svc, _ := newService(t, &recorder{}, pending())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	reason, err := svc.FollowLogs(ctx, "sandbox1", &out)
	if err != nil || reason != "" {
		t.Errorf("FollowLogs ended with %q, %v, want no reason and no error", reason, err)
	}
}

// A start that fails under the follow fails the record, and the follow ends with what a new logs call answers.
func TestFollowEndsWithTheRefusalWhenTheSandboxFailsUnderIt(t *testing.T) {
	var out bytes.Buffer

	created := running()
	created.State = models.StateCreated
	svc, l, _ := logsOf(t, &recorder{}, created, "")
	l.provider.exits = func() { failedCreate(&l.repo.sb) }

	_, err := svc.FollowLogs(t.Context(), "sandbox1", &out)
	refusedAsFailed(t, err)
}
