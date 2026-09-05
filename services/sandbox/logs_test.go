package sandbox_test

import (
	"bytes"
	"context"
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

	if err := svc.Logs(t.Context(), "sandbox1", false, &out); err != nil {
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

	if err := svc.Logs(t.Context(), "sandbox1", false, &out); err != nil {
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

	if err := svc.Logs(t.Context(), "web", false, &out); err != nil {
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

	err := svc.Logs(t.Context(), "sandbox1", false, &out)
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("Logs returned %v, want the id named", err)
	}
}

// A follow drains what arrives after it started, and ends on its own once the sandbox is gone.
func TestFollowDrainsThenEndsWhenTheSandboxIsGone(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	svc, l, path := logsOf(t, r, running(), "first\n")
	l.provider.exits = func() { appendTo(t, path, "second\n") }

	if err := svc.Logs(t.Context(), "sandbox1", true, &out); err != nil {
		t.Fatalf("Logs -f: %v", err)
	}
	if out.String() != "first\nsecond\n" {
		t.Errorf("Logs -f printed %q, want both lines", out.String())
	}
}

// An interrupt is how an operator leaves a follow on a sandbox that is still up, and it is no error.
func TestFollowLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	svc, _, _ := logsOf(t, &recorder{}, running(), "up\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := svc.Logs(ctx, "sandbox1", true, &out); err != nil {
		t.Fatalf("Logs -f: %v", err)
	}
	if out.String() != "up\n" {
		t.Errorf("Logs -f printed %q", out.String())
	}
}

func TestFollowReportsASubstrateItCannotAsk(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Status"}}
	svc, _, _ := logsOf(t, r, running(), "")

	if err := svc.Logs(t.Context(), "sandbox1", true, &out); err == nil {
		t.Fatal("Logs -f returned no error for a substrate it could not ask")
	}
}
