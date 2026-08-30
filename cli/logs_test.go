package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestParseLogsFlags(t *testing.T) {
	opts, err := parseLogs([]string{"-f", "sandbox1"})
	if err != nil {
		t.Fatalf("parseLogs: %v", err)
	}
	if opts.id != "sandbox1" || !opts.follow {
		t.Errorf("parseLogs gave %+v, want sandbox1 and follow", opts)
	}

	for name, args := range map[string][]string{
		"no id":           {},
		"two ids":         {"sandbox1", "sandbox2"},
		"a flag after id": {"sandbox1", "-f"},
		"an unknown flag": {"--tail", "sandbox1"},
	} {
		if _, err := parseLogs(args); err == nil {
			t.Errorf("parseLogs(%s) returned no error", name)
		}
	}
}

// newLogsApp wires logs onto fakes with an output file that already holds what the entrypoint wrote.
func newLogsApp(t *testing.T, out *bytes.Buffer, sb models.Sandbox, written string) (App, *deps, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatalf("write the output file: %v", err)
	}

	app, deps := newLifecycleApp(t, out, &recorder{}, sb)
	deps.providerSvc.(*fakeLifecycleProvider).logPath = path

	return app, deps, path
}

func TestLogsPrintsWhatTheEntrypointWrote(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := newLogsApp(t, &out, running(), "hello\nworld\n")

	if err := app.Run(t.Context(), []string{"logs", "sandbox1"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if out.String() != "hello\nworld\n" {
		t.Errorf("logs printed %q", out.String())
	}
}

// The output outlives the entrypoint: a stopped sandbox still answers with everything it wrote.
func TestLogsReadsAStoppedSandbox(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped
	app, _, _ := newLogsApp(t, &out, sb, "done\n")

	if err := app.Run(t.Context(), []string{"logs", "sandbox1"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if out.String() != "done\n" {
		t.Errorf("logs printed %q", out.String())
	}
}

func TestLogsTakesAName(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.Name = "web"
	app, deps, _ := newLogsApp(t, &out, sb, "named\n")

	if err := app.Run(t.Context(), []string{"logs", "web"}); err != nil {
		t.Fatalf("logs web: %v", err)
	}
	if out.String() != "named\n" {
		t.Errorf("logs printed %q", out.String())
	}
	if calls := deps.providerSvc.(*fakeLifecycleProvider).r.calls; !slices.Contains(calls, "provider.LogPath") {
		t.Errorf("logs never asked the provider for the path: %v", calls)
	}
}

func TestLogsRefusesAnIDThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	app, deps, _ := newLogsApp(t, &out, running(), "")
	deps.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"logs", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("logs returned %v, want the id named", err)
	}
}

// exitingProvider is up on the first Status, then writes its last line and is gone on the second.
type exitingProvider struct {
	models.Provider

	t     *testing.T
	path  string
	calls int
}

func (p *exitingProvider) Status(context.Context, string) (models.Status, error) {
	p.calls++
	if p.calls == 1 {
		return models.Status{Exists: true, State: models.StateRunning}, nil
	}

	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		p.t.Fatalf("open the output file: %v", err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		p.t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		p.t.Fatalf("close: %v", err)
	}

	return models.Status{}, nil
}

// A follow drains what arrives after it started, and ends on its own once the sandbox is gone.
func TestFollowDrainsThenEndsWhenTheSandboxIsGone(t *testing.T) {
	var out bytes.Buffer

	_, _, path := newLogsApp(t, &out, running(), "first\n")

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the output file: %v", err)
	}
	defer f.Close()

	if err := follow(t.Context(), &out, f, &exitingProvider{t: t, path: path}, "sandbox1"); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if out.String() != "first\nsecond\n" {
		t.Errorf("follow printed %q, want both lines", out.String())
	}
}

// An interrupt is how an operator leaves a follow on a sandbox that is still up, and it is no error.
func TestFollowLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := newLogsApp(t, &out, running(), "up\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := app.Run(ctx, []string{"logs", "-f", "sandbox1"}); err != nil {
		t.Fatalf("logs -f: %v", err)
	}
	if out.String() != "up\n" {
		t.Errorf("logs -f printed %q", out.String())
	}
}

func TestFollowReportsASubstrateItCannotAsk(t *testing.T) {
	var out bytes.Buffer

	app, deps, _ := newLogsApp(t, &out, running(), "")
	deps.providerSvc.(*fakeLifecycleProvider).r.fail = []string{"provider.Status"}

	if err := app.Run(t.Context(), []string{"logs", "-f", "sandbox1"}); err == nil {
		t.Fatal("logs -f returned no error for a substrate it could not ask")
	}
}
