package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// newRunApp is a daemon on fakes whose app has written output into the log the attach follows.
func newRunApp(t *testing.T, out *bytes.Buffer, output string) (App, *fakeDaemon, *recorder) {
	t.Helper()

	app, d, r := newDaemonCreateApp(t, out)
	logPath := filepath.Join(t.TempDir(), "sandbox.log")
	if err := os.WriteFile(logPath, []byte(output), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}
	d.providerSvc.(*fakeLifecycleProvider).logPath = logPath

	return app, d, r
}

func TestRunPrintsTheAppOutputAndExitsWithItsCode(t *testing.T) {
	var out bytes.Buffer

	app, d, _ := newRunApp(t, &out, "ready\n")
	provider := d.providerSvc.(*fakeLifecycleProvider)
	provider.endApp(models.ExitStatus{Code: 3}, 2)

	err := app.Run(t.Context(), []string{"run", "--restart", "on-failure", "alpine:3.20", "sh", "-c", "exit 3"})

	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 || exit.Message != "" {
		t.Fatalf("run returned %v, want the app's code 3 and no message", err)
	}
	if got := out.String(); got != "ready\n" {
		t.Errorf("run printed %q, want the app's output once", got)
	}
	if want := []string{"sh", "-c", "exit 3"}; !slices.Equal(provider.created.Entrypoint, want) {
		t.Errorf("the substrate got %v as the app, want %v", provider.created.Entrypoint, want)
	}
}

func TestRunExitsZeroForAnAppThatSucceeded(t *testing.T) {
	var out bytes.Buffer

	app, d, _ := newRunApp(t, &out, "")
	d.providerSvc.(*fakeLifecycleProvider).endApp(models.ExitStatus{}, 0)

	if err := app.Run(t.Context(), []string{"run", "alpine:3.20", "true"}); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
}

// -d is for a script that wants the id and not the app, so it prints the id once the sandbox is up and attaches to nothing.
func TestRunDetachedPrintsTheIDAndAttachesToNothing(t *testing.T) {
	var out bytes.Buffer

	app, _, r := newRunApp(t, &out, "never printed\n")

	if err := app.Run(t.Context(), []string{"run", "-d", "alpine:3.20", "sleep", "60"}); err != nil {
		t.Fatalf("run -d: %v", err)
	}
	if got := out.String(); got != "sandbox2\n" {
		t.Errorf("run -d printed %q, want the bare id", got)
	}
	if slices.Contains(r.seen(), "provider.LogPath") {
		t.Errorf("run -d drove %v, want no attach", r.seen())
	}
}

// A shard failure is 125, so a script tells it from an app that exited 1.
func TestRunExitsWith125WhenShardFails(t *testing.T) {
	var out bytes.Buffer

	app, _, r := newRunApp(t, &out, "")
	r.fail = []string{"images.Pull"}

	err := app.Run(t.Context(), []string{"run", "alpine:3.20", "true"})

	var exit *ExitError
	// The create route is public, so the cause stays in the daemon log.
	if !errors.As(err, &exit) || exit.Code != runFailedExitCode || !strings.Contains(exit.Message, "the daemon log has the cause") || strings.Contains(exit.Message, "forced failure") {
		t.Fatalf("run returned %v, want 125 with the public text", err)
	}
}

// A script reads an app that never ran as a shell does, 127 or 126, never as shard failing with 125.
func TestRunExitsWithTheCodeOfAnAppThatNeverStarted(t *testing.T) {
	var out bytes.Buffer

	app, d, r := newRunApp(t, &out, "")
	d.providerSvc.(*fakeLifecycleProvider).startErr = &models.CommandNotStartedError{Sandbox: "sandbox2", Reason: "no such file or directory", Code: models.CommandNotFoundExitCode}

	err := app.Run(t.Context(), []string{"run", "alpine:3.20", "no-such-app"})

	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != models.CommandNotFoundExitCode || !strings.Contains(exit.Message, `could not run "no-such-app"`) {
		t.Fatalf("run returned %v, want %d with the command named", err, models.CommandNotFoundExitCode)
	}
	if !slices.Contains(r.seen(), "provider.Remove") {
		t.Errorf("the daemon drove %v, want the sandbox removed behind the refusal", r.seen())
	}
}

// startRun runs shard run with interrupts main would route, and answers once the run took them.
func startRun(t *testing.T, app App, args ...string) (chan<- os.Signal, <-chan error) {
	t.Helper()

	signals, done := goRun(t, &app, args...)
	waitFor(t, "the run to take the interrupts", func() bool { return app.Interrupts.receiver() != nil })

	return signals, done
}

func goRun(t *testing.T, app *App, args ...string) (chan<- os.Signal, <-chan error) {
	t.Helper()

	signals := make(chan os.Signal, 3)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	app.Interrupts = NewInterrupts(signals, cancel, func() { t.Error("the process left on a signal the run took") })
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

func TestRunStopsTheAppOnTheFirstInterruptAndExitsWithItsCode(t *testing.T) {
	var out bytes.Buffer

	app, d, r := newRunApp(t, &out, "")
	provider := d.providerSvc.(*fakeLifecycleProvider)
	provider.endOnStop = &models.ExitStatus{Code: 143, Signal: 15}

	signals, done := startRun(t, app, "--restart", "always", "alpine:3.20", "sleep", "60")
	// An interrupt during the create takes cancelApp's path, which exits 130, so the press waits for the attach.
	waitFor(t, "the attach", func() bool { return slices.Contains(r.seen(), "provider.LogPath") })
	signals <- syscall.SIGINT

	err := <-done
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 143 {
		t.Fatalf("run returned %v, want the app's 143", err)
	}
	if got := provider.stops(); !slices.Equal(got, []bool{false}) {
		t.Errorf("run asked for stops %v, want one without force", got)
	}
	if !strings.Contains(out.String(), "shard: stopping the main command; Ctrl+C again to kill it") {
		t.Errorf("run printed %q, want the note on the stop", out.String())
	}
	if slices.Contains(r.seen(), "provider.Stop") {
		t.Errorf("run stopped the sandbox, want it running with only shard-init")
	}
}

// An app that ignores TERM keeps the run attached: the second interrupt kills it and the third leaves.
func TestRunKillsOnTheSecondInterruptAndLeavesOnTheThird(t *testing.T) {
	var out bytes.Buffer

	app, d, _ := newRunApp(t, &out, "")
	provider := d.providerSvc.(*fakeLifecycleProvider)

	signals, done := startRun(t, app, "alpine:3.20", "sleep", "60")
	signals <- syscall.SIGINT
	waitFor(t, "the stop", func() bool { return len(provider.stops()) == 1 })
	signals <- syscall.SIGINT
	waitFor(t, "the kill", func() bool { return len(provider.stops()) == 2 })
	signals <- syscall.SIGINT

	err := <-done
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != InterruptedExitCode || exit.Message != "left the run; sandbox sandbox2 stays running" {
		t.Fatalf("run returned %v, want 130 that names the sandbox", err)
	}
	if got := provider.stops(); !slices.Equal(got, []bool{false, true}) {
		t.Errorf("run asked for stops %v, want a term then a kill", got)
	}
	if !strings.Contains(out.String(), "shard: killing the main command; Ctrl+C again to leave") {
		t.Errorf("run printed %q, want the note on the kill", out.String())
	}
}

// The daemon starts the app of a create its caller left, so a Ctrl+C before the sandbox is up stops the app once it is, and the run never leaves before.
func TestRunStopsTheAppOfACreateItInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uncached bool
		detach   bool
		presses  int
		note     string
		stops    []bool
	}{
		{name: "cached", presses: 1, note: "stopping the main command once the sandbox is up; Ctrl+C again to kill it", stops: []bool{false}},
		{name: "cached detached", detach: true, presses: 1, note: "stopping the main command once the sandbox is up; Ctrl+C again to kill it", stops: []bool{false}},
		{name: "cached pressed three times", presses: 3, note: "killing the main command once the sandbox is up", stops: []bool{true}},
		{name: "uncached", uncached: true, presses: 1, note: "stopping the main command once the sandbox is up; Ctrl+C again to kill it", stops: []bool{false}},
		{name: "uncached detached", uncached: true, detach: true, presses: 1, note: "stopping the main command once the sandbox is up; Ctrl+C again to kill it", stops: []bool{false}},
		{name: "uncached pressed three times", uncached: true, presses: 3, note: "killing the main command once the sandbox is up", stops: []bool{true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			app, d, r := newRunApp(t, &out, "app output\n")
			if tc.uncached {
				d.creates = newBackgroundCreates(t)
			}
			provider := d.providerSvc.(*fakeLifecycleProvider)
			provider.endOnStop = &models.ExitStatus{Code: 143, Signal: 15}
			provider.startGate = make(chan struct{})
			release := sync.OnceFunc(func() { close(provider.startGate) })
			t.Cleanup(release)
			notes := &syncBuffer{}
			app.Err = notes

			args := []string{"alpine:3.20", "sleep", "60"}
			if tc.detach {
				args = append([]string{"-d"}, args...)
			}
			signals, done := goRun(t, &app, args...)
			waitFor(t, "the start", func() bool { return slices.Contains(r.seen(), "provider.Start") })
			for range tc.presses {
				signals <- syscall.SIGINT
			}
			waitFor(t, "the note on every press, or the run to end", func() bool {
				return strings.Count(notes.String(), "shard: ") == tc.presses || len(done) > 0
			})
			if len(done) > 0 {
				t.Fatalf("run returned %v before the sandbox was up", <-done)
			}
			if !strings.Contains(notes.String(), "shard: "+tc.note+"\n") {
				t.Errorf("run noted %q, want %q last", notes.String(), tc.note)
			}
			release()

			err := <-done
			var exit *ExitError
			if !errors.As(err, &exit) || exit.Code != InterruptedExitCode || exit.Message != "interrupted; the main command of sandbox sandbox2 ended, and the sandbox stays running" {
				t.Fatalf("run returned %v, want 130 once the app ended", err)
			}
			if got := provider.stops(); !slices.Equal(got, tc.stops) {
				t.Errorf("run asked for stops %v, want %v", got, tc.stops)
			}
			if tc.detach == strings.Contains(out.String(), "app output") {
				t.Errorf("run with detach %v printed %q", tc.detach, out.String())
			}
		})
	}
}
