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

func TestParseProcessLogsFlags(t *testing.T) {
	for args, want := range map[string]processLogsOptions{
		"-f sandbox1":           {id: "sandbox1", follow: true},
		"sandbox1 api -f":       {id: "sandbox1", name: "api", follow: true},
		"sandbox1 --follow api": {id: "sandbox1", name: "api", follow: true},
		"sandbox1":              {id: "sandbox1"},
	} {
		opts, err := parseProcessLogs(strings.Fields(args))
		if err != nil {
			t.Fatalf("parseProcessLogs(%q): %v", args, err)
		}
		if opts != want {
			t.Errorf("parseProcessLogs(%q) gave %+v, want %+v", args, opts, want)
		}
	}

	for name, args := range map[string][]string{
		"no id":            {},
		"a third argument": {"sandbox1", "api", "worker"},
		"an unknown flag":  {"--tail", "sandbox1"},
	} {
		if _, err := parseProcessLogs(args); err == nil {
			t.Errorf("parseProcessLogs(%s) returned no error", name)
		}
	}
}

// newLogsApp wires logs onto a daemon whose sandbox ran the named processes, and whose log already holds what one wrote.
func newLogsApp(t *testing.T, out *bytes.Buffer, sb models.Sandbox, written string, names ...string) (App, *fakeDaemon) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatalf("write the output file: %v", err)
	}

	for _, name := range names {
		sb.Processes = append(sb.Processes, models.Process{Name: name, Command: []string{name}, Status: models.ProcessStatus{State: models.ProcessRunning}})
	}
	app, d := newClientApp(t, out, sb)
	d.providerSvc.(*fakeLifecycleProvider).logPath = path

	return app, d
}

func TestLogsPrintsWhatTheOnlyProcessWrote(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, running(), "hello\nworld\n", "api")

	if err := app.Run(t.Context(), []string{"logs", "sandbox1"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if out.String() != "hello\nworld\n" {
		t.Errorf("logs printed %q", out.String())
	}
}

// The output outlives the process: a stopped sandbox still answers with everything it wrote.
func TestLogsReadsAStoppedSandbox(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, stopped(), "done\n", "api")

	if err := app.Run(t.Context(), []string{"logs", "web"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if out.String() != "done\n" {
		t.Errorf("logs printed %q", out.String())
	}
}

func TestLogsTakesAProcessName(t *testing.T) {
	var out bytes.Buffer

	app, d := newLogsApp(t, &out, stopped(), "named\n", "api", "worker")

	if err := app.Run(t.Context(), []string{"logs", "web", "worker"}); err != nil {
		t.Fatalf("logs web worker: %v", err)
	}
	if out.String() != "named\n" {
		t.Errorf("logs printed %q", out.String())
	}
	if calls := d.providerSvc.(*fakeLifecycleProvider).r.seen(); !slices.Contains(calls, "provider.ProcessLogPath") {
		t.Errorf("logs never asked the provider for the path: %v", calls)
	}
}

// Without a name logs means the one process, so a sandbox with none or several says what to type instead.
func TestLogsWithoutANameNeedsExactlyOneProcess(t *testing.T) {
	for want, names := range map[string][]string{
		"sandbox web has no process to show the output of; shard run web -- COMMAND starts one":       nil,
		"sandbox web has 2 processes, so name one: shard logs web NAME, with NAME one of api, worker": {"api", "worker"},
	} {
		var out bytes.Buffer

		app, _ := newLogsApp(t, &out, stopped(), "", names...)

		err := app.Run(t.Context(), []string{"logs", "web"})
		if err == nil || err.Error() != want {
			t.Errorf("logs with %v returned %v, want %q", names, err, want)
		}
	}
}

func TestLogsRefusesAProcessTheSandboxNeverRan(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, stopped(), "", "api")

	err := app.Run(t.Context(), []string{"logs", "web", "ghost"})
	if err == nil || !strings.Contains(err.Error(), "no process ghost") {
		t.Errorf("logs returned %v, want the process named", err)
	}
}

func TestLogsRefusesAnIDThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	app, d := newLogsApp(t, &out, running(), "", "api")
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"logs", "sandbox1", "api"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("logs returned %v, want the id named", err)
	}
}

// An interrupt is how an operator leaves a follow on a process that is still up, and it is no error.
func TestLogsFollowLeavesOnAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, running(), "up\n", "api")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := app.Run(ctx, []string{"logs", "-f", "sandbox1", "api"}); err != nil {
		t.Fatalf("logs -f: %v", err)
	}
}

// A daemon nothing answers on is the one failure every verb prints the same way.
func TestLogsReportsADaemonThatIsNotThere(t *testing.T) {
	var out bytes.Buffer

	app := App{Version: "test", Root: shortRoot(t), Out: &out}

	err := app.Run(t.Context(), []string{"logs", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "cannot connect to the shard daemon") {
		t.Fatalf("logs with no daemon returned %v", err)
	}
}

// The egress log moved to shard policy logs, and logs keeps no alias for it.
func TestLogsRefusesTheEgressFlag(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLogsApp(t, &out, running(), "", "api")

	err := app.Run(t.Context(), []string{"logs", "--egress", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag --egress") {
		t.Errorf("logs --egress returned %v, want an unknown flag", err)
	}
	if out.Len() != 0 {
		t.Errorf("logs --egress printed %q", out.String())
	}
}
