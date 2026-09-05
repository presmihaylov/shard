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

// newLogsApp wires logs onto a daemon whose output file already holds what the entrypoint wrote.
func newLogsApp(t *testing.T, out *bytes.Buffer, sb models.Sandbox, written string) (App, *deps) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatalf("write the output file: %v", err)
	}

	app, d := newClientApp(t, out, sb)
	d.providerSvc.(*fakeLifecycleProvider).logPath = path

	return app, d
}

func TestLogsPrintsWhatTheEntrypointWrote(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, running(), "hello\nworld\n")

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
	app, _ := newLogsApp(t, &out, sb, "done\n")

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
	app, d := newLogsApp(t, &out, sb, "named\n")

	if err := app.Run(t.Context(), []string{"logs", "web"}); err != nil {
		t.Fatalf("logs web: %v", err)
	}
	if out.String() != "named\n" {
		t.Errorf("logs printed %q", out.String())
	}
	if calls := d.providerSvc.(*fakeLifecycleProvider).r.calls; !slices.Contains(calls, "provider.LogPath") {
		t.Errorf("logs never asked the provider for the path: %v", calls)
	}
}

func TestLogsRefusesAnIDThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	app, d := newLogsApp(t, &out, running(), "")
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"logs", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("logs returned %v, want the id named", err)
	}
}

// An interrupt is how an operator leaves a follow on a sandbox that is still up, and it is no error.
func TestLogsFollowLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, running(), "up\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := app.Run(ctx, []string{"logs", "-f", "sandbox1"}); err != nil {
		t.Fatalf("logs -f: %v", err)
	}
}

// A daemon nothing answers on is the one failure every verb prints the same way.
func TestLogsReportsADaemonThatIsNotThere(t *testing.T) {
	var out bytes.Buffer

	app := App{Version: "test", Root: shortRoot(t), Out: &out}

	err := app.Run(t.Context(), []string{"logs", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "cannot connect to shard daemon") {
		t.Fatalf("logs with no daemon returned %v", err)
	}
}
