package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// newRunApp is a daemon on fakes with one running sandbox, whose process log already holds an earlier run's output.
func newRunApp(t *testing.T, out *bytes.Buffer) (App, *fakeLifecycleProvider, *recorder) {
	t.Helper()

	r := &recorder{}
	app, d := newLifecycleApp(t, out, r, running())
	provider := d.providerSvc.(*fakeLifecycleProvider)
	provider.logPath = filepath.Join(t.TempDir(), "process.log")
	if err := os.WriteFile(provider.logPath, []byte("an earlier run\n"), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	return app, provider, r
}

// The name is the output, so a script takes it: name=$(shard run web -- python app.py).
func TestRunPrintsTheProcessName(t *testing.T) {
	var out bytes.Buffer

	app, provider, r := newRunApp(t, &out)

	if err := app.Run(t.Context(), []string{"run", "sandbox1", "--", "/usr/bin/sleep", "60"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "sleep\n" {
		t.Errorf("run printed %q, want the name the command gave the process", got)
	}

	want := []models.ProcessSpec{{Name: "sleep", Argv: []string{"/usr/bin/sleep", "60"}, Restart: models.RestartSpec{Policy: models.RestartUnlessStopped, Backoff: 1}}}
	if got := provider.runs(); !reflect.DeepEqual(got, want) {
		t.Errorf("the guest started %+v, want %+v", got, want)
	}
	// Run reads the log size once for where this run starts, and an attach would open the log a second time.
	if got := keep(r.seen(), "provider.ProcessLogPath"); len(got) != 1 {
		t.Errorf("run drove %v, want no attach", r.seen())
	}
}

func TestRunHandsTheGuestEveryFlag(t *testing.T) {
	var out bytes.Buffer

	app, provider, _ := newRunApp(t, &out)

	args := []string{"run", "sandbox1", "--name", "api", "-e", "A=1", "--env", "B=2", "-w", "/srv", "-u", "nobody", "--restart", "on-failure", "--restart-retries", "2", "--restart-backoff", "3s", "python", "-m", "http.server"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := out.String(); got != "api\n" {
		t.Errorf("run printed %q, want the name it was given", got)
	}

	want := []models.ProcessSpec{{
		Name:    "api",
		Argv:    []string{"python", "-m", "http.server"},
		Env:     []string{"A=1", "B=2"},
		WorkDir: "/srv",
		User:    "nobody",
		Restart: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 3},
	}}
	if got := provider.runs(); !reflect.DeepEqual(got, want) {
		t.Errorf("the guest started %+v, want %+v", got, want)
	}
}

// With --attach stdout is the current run's output alone, and the exit is the process's, as a shell gives it.
func TestRunAttachedPrintsTheOutputAndExitsWithTheCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit models.ExitStatus
		want int
	}{
		{name: "success", exit: models.ExitStatus{}, want: 0},
		{name: "failure", exit: models.ExitStatus{Code: 3}, want: 3},
		{name: "signal", exit: models.ExitStatus{Code: 137, Signal: 9}, want: 137},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			app, provider, _ := newRunApp(t, &out)
			var stderr bytes.Buffer
			app.Err = &stderr
			provider.output = "ready\n"
			provider.endOnStart = &tc.exit

			err := app.Run(t.Context(), []string{"run", "sandbox1", "--attach", "--", "sh", "-c", "exit 3"})

			var exit *ExitError
			if tc.want == 0 && err != nil {
				t.Fatalf("run returned %v, want nil", err)
			}
			if tc.want != 0 && (!errors.As(err, &exit) || exit.Code != tc.want || exit.Message != "") {
				t.Fatalf("run returned %v, want code %d and no message", err, tc.want)
			}
			if got := out.String(); got != "ready\n" {
				t.Errorf("run printed %q, want this run's output alone", got)
			}
			if got := stderr.String(); got != "" {
				t.Errorf("run wrote %q to stderr, want nothing", got)
			}
		})
	}
}

func goRun(t *testing.T, app *App, args ...string) (chan<- os.Signal, <-chan error) {
	t.Helper()

	signals := make(chan os.Signal, 3)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	app.Interrupts = NewInterrupts(signals, cancel, func() { t.Error("the process left on a single interrupt") })
	go app.Interrupts.Watch()

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, append([]string{"run"}, args...)) }()

	return signals, done
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One Ctrl+C leaves the attach and never the process, which only shard kill ends.
func TestRunAttachedDetachesOnOneInterrupt(t *testing.T) {
	var out bytes.Buffer

	app, provider, r := newRunApp(t, &out)
	notes := &syncBuffer{}
	app.Err = notes

	signals, done := goRun(t, &app, "sandbox1", "--attach", "--name", "api", "--", "sleep", "60")
	waitFor(t, "the attach", func() bool { return len(keep(r.seen(), "provider.ProcessLogPath")) == 2 })
	signals <- syscall.SIGINT

	if err := <-done; err != nil {
		t.Fatalf("run returned %v, want nil once it detached", err)
	}
	if want := "shard: detached; process api runs on, and shard kill sandbox1 api stops it\n"; notes.String() != want {
		t.Errorf("run noted %q, want %q", notes.String(), want)
	}
	if got := provider.killed(); len(got) != 0 {
		t.Errorf("run killed the process with graces %v, want it left running", got)
	}
}

// A shard failure is 125, so a script tells it from a process that exited 1.
func TestRunExitsWith125WhenShardFails(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, _ := newLifecycleApp(t, &out, r, stopped())

	err := app.Run(t.Context(), []string{"run", "web", "--", "true"})

	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != runFailedExitCode || !strings.Contains(exit.Message, "web") {
		t.Fatalf("run returned %v, want 125 that names the sandbox", err)
	}
	if slices.Contains(r.seen(), "provider.StartProcess") {
		t.Errorf("run drove %v, want no process started in a stopped sandbox", r.seen())
	}
}

// A script reads a command that never ran as a shell does, 127 or 126, never as shard failing with 125.
func TestRunExitsWithTheCodeOfACommandThatNeverStarted(t *testing.T) {
	for _, attach := range []bool{false, true} {
		var out bytes.Buffer

		app, provider, _ := newRunApp(t, &out)
		provider.startProcessErr = &models.CommandNotStartedError{Sandbox: "sandbox1", Reason: "no such file or directory", Code: models.CommandNotFoundExitCode}

		args := []string{"run", "sandbox1", "--", "no-such-app"}
		if attach {
			args = slices.Insert(args, 2, "--attach")
		}
		err := app.Run(t.Context(), args)

		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != models.CommandNotFoundExitCode || !strings.Contains(exit.Message, `could not run "no-such-app"`) {
			t.Errorf("run with attach %v returned %v, want %d with the command named", attach, err, models.CommandNotFoundExitCode)
		}
	}
}

func TestParseRunTakesFlagsOnEitherSideOfTheSandbox(t *testing.T) {
	for _, args := range [][]string{
		{"--name", "api", "-e", "A=1", "web", "--", "python", "-m", "http.server"},
		{"web", "--name", "api", "-e", "A=1", "python", "-m", "http.server"},
		{"--name", "api", "web", "-e", "A=1", "--", "python", "-m", "http.server"},
	} {
		opts, err := parseRun(args)
		if err != nil {
			t.Fatalf("parseRun(%v): %v", args, err)
		}
		if opts.id != "web" || opts.req.Name != "api" || !slices.Equal(opts.req.Env, []string{"A=1"}) || !slices.Equal(opts.req.Command, []string{"python", "-m", "http.server"}) {
			t.Errorf("parseRun(%v) = %+v, want process api of web", args, opts)
		}
	}
}

func TestParseRunPreservesArguments(t *testing.T) {
	command := []string{"sh", "-c", "echo ready", "", "--", "--help", "--name", "guest", "-it"}
	for _, separator := range [][]string{nil, {"--"}} {
		args := []string{"--name", "lab", "web"}
		args = append(args, separator...)
		args = append(args, command...)
		opts, err := parseRun(args)
		if err != nil {
			t.Fatalf("parseRun: %v", err)
		}
		if opts.req.Name != "lab" || opts.id != "web" || !slices.Equal(opts.req.Command, command) {
			t.Errorf("parseRun = %+v, want the guest arguments intact", opts)
		}
	}
}

func TestParseRunTakesACommandThatStartsWithAHyphenAfterTheSeparator(t *testing.T) {
	opts, err := parseRun([]string{"web", "--", "--guest", "-it"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	if !slices.Equal(opts.req.Command, []string{"--guest", "-it"}) {
		t.Errorf("command = %v, want the guest command intact", opts.req.Command)
	}
}

func TestParseRunNeedsASandboxAndACommand(t *testing.T) {
	for args, want := range map[string]string{
		"":                 "run takes one sandbox id or name, got none",
		"--attach":         "run takes one sandbox id or name, got none",
		"web":              "run takes a command after the sandbox id or name: shard run SANDBOX [OPTIONS] [--] COMMAND [ARGS...]",
		"web --":           "run takes a command after the sandbox id or name: shard run SANDBOX [OPTIONS] [--] COMMAND [ARGS...]",
		"--name= web true": "--name needs a process name; leave it out to name the process after its command",
	} {
		if _, err := parseRun(strings.Fields(args)); err == nil || err.Error() != want {
			t.Errorf("parseRun(%q) = %v, want %q", args, err, want)
		}
	}
}

func TestParseRunRestartFlags(t *testing.T) {
	for args, want := range map[string]*models.RestartSpec{
		"--restart on-failure --restart-retries 2 --restart-backoff 3s web true": {Policy: models.RestartOnFailure, Retries: 2, Backoff: 3},
		"--restart always web true": {Policy: models.RestartAlways},
		"web true":                  nil,
	} {
		opts, err := parseRun(strings.Fields(args))
		if err != nil {
			t.Fatalf("parseRun(%q): %v", args, err)
		}
		if !reflect.DeepEqual(opts.req.Restart, want) {
			t.Errorf("parseRun(%q) restart = %+v, want %+v with the rest left for the daemon's defaults", args, opts.req.Restart, want)
		}
	}
}

func TestParseRunRejections(t *testing.T) {
	cases := map[string][]string{
		"a policy setting alone":    {"--restart-retries", "2", "web", "true"},
		"a negative start count":    {"--restart", "on-failure", "--restart-retries", "-1", "web", "true"},
		"always with a start count": {"--restart", "always", "--restart-retries", "2", "web", "true"},
		"a sub-second backoff":      {"--restart", "always", "--restart-backoff", "500ms", "web", "true"},
		"an env with no value":      {"-e", "DEBUG", "web", "true"},
		"a create flag":             {"--memory", "512MiB", "web", "true"},
		"the old detach":            {"-d", "web", "true"},
	}

	for name, args := range cases {
		if _, err := parseRun(args); err == nil {
			t.Errorf("parseRun(%s) returned no error", name)
		}
	}
}
