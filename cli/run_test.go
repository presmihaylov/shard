package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	if !errors.As(err, &exit) || exit.Code != runFailedExitCode || !strings.Contains(exit.Message, "forced failure at images.Pull") {
		t.Fatalf("run returned %v, want 125 with the daemon's reason", err)
	}
}

// startRun runs shard run with interrupts main would route, and answers once the attach took them.
func startRun(t *testing.T, app App, args ...string) (chan<- os.Signal, <-chan error) {
	t.Helper()

	signals := make(chan os.Signal, 3)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	app.Interrupts = NewInterrupts(signals, cancel, func() { t.Error("the process left on a signal the run took") })
	go app.Interrupts.Watch()

	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, append([]string{"run"}, args...)) }()

	waitFor(t, "the run to take the interrupts", func() bool { return app.Interrupts.receiver() != nil })

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
	signals <- syscall.SIGINT

	err := <-done
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 143 {
		t.Fatalf("run returned %v, want the app's 143", err)
	}
	if got := provider.stops(); !slices.Equal(got, []bool{false}) {
		t.Errorf("run asked for stops %v, want one without force", got)
	}
	if !strings.Contains(out.String(), "shard: stopping the app; Ctrl+C again to kill it") {
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
	if !strings.Contains(out.String(), "shard: killing the app; Ctrl+C again to leave") {
		t.Errorf("run printed %q, want the note on the kill", out.String())
	}
}
