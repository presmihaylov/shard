package sandbox_test

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// withApp is a running sandbox that shard run started, so it has an app to attach to and stop.
func withApp() models.Sandbox {
	sb := running()
	sb.Command = []string{"sh", "-c", "exit 4"}

	return sb
}

func appCode(err error) models.Code {
	var refused *sandbox.StateError
	if !errors.As(err, &refused) {
		return ""
	}

	return refused.Code
}

func TestAttachAppCopiesTheOutputUntilTheAppEnds(t *testing.T) {
	var out bytes.Buffer

	svc, l, path := logsOf(t, &recorder{}, withApp(), "first run\n")
	l.provider.appEnds = func() {
		appendTo(t, path, "last run\n")
		l.provider.entrypointExit = &models.ExitStatus{Code: 4}
		l.provider.restarts = models.RestartCount{Count: 2, GaveUp: true, Ended: true}
	}

	exit, err := svc.AttachApp(t.Context(), "sandbox1", func() (io.Writer, error) { return &out, nil })
	if err != nil {
		t.Fatalf("AttachApp: %v", err)
	}
	if want := (models.AppExit{Code: 4, Restarts: 2}); exit != want {
		t.Errorf("AttachApp answered %+v, want %+v", exit, want)
	}
	if out.String() != "first run\nlast run\n" {
		t.Errorf("AttachApp copied %q, want every run's output once", out.String())
	}
}

// A signal death answers 128+n as the code, the way a shell reports it, with the signal beside it.
func TestWaitAppAnswersHowTheAppEnded(t *testing.T) {
	svc, l := newService(t, &recorder{}, withApp())
	l.provider.entrypointExit = &models.ExitStatus{Code: 137, Signal: 9}
	l.provider.restarts = models.RestartCount{Ended: true}

	exit, err := svc.WaitApp(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("WaitApp: %v", err)
	}
	if want := (models.AppExit{Code: 137, Signal: 9}); exit != want {
		t.Errorf("WaitApp answered %+v, want %+v", exit, want)
	}
}

// inspect reads the record, so the end of the policy is on it when the run returns and not a tick later.
func TestWaitAppRecordsTheEndOfThePolicy(t *testing.T) {
	sb := withApp()
	sb.Restart = &models.Restart{RestartSpec: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 1}}
	svc, l := newService(t, &recorder{}, sb)
	l.provider.entrypointExit = &models.ExitStatus{Code: 4}
	l.provider.restarts = models.RestartCount{Count: 2, GaveUp: true, Ended: true}

	if _, err := svc.WaitApp(t.Context(), "sandbox1"); err != nil {
		t.Fatalf("WaitApp: %v", err)
	}

	if got := l.repo.sb; got.Restart == nil || got.Restart.RestartCount != l.provider.restarts {
		t.Errorf("the record holds %+v, want the count the supervisor ended on", got.Restart)
	}
}

// A sandbox create made runs only shard-init, so there is no app to attach to or stop, and the refusal comes before the attach answers.
func TestTheAppVerbsRefuseASandboxWithNoApp(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())

	opened := false
	_, attachErr := svc.AttachApp(t.Context(), "sandbox1", func() (io.Writer, error) {
		opened = true

		return io.Discard, nil
	})
	_, waitErr := svc.WaitApp(t.Context(), "sandbox1")
	stopErr := svc.StopApp(t.Context(), "sandbox1", false)

	for verb, err := range map[string]error{"AttachApp": attachErr, "WaitApp": waitErr, "StopApp": stopErr} {
		if got := appCode(err); got != models.CodeNoApp {
			t.Errorf("%s returned %v, want %s", verb, err, models.CodeNoApp)
		}
	}
	if opened {
		t.Error("AttachApp answered before it refused")
	}
	if len(l.provider.stopApps) != 0 {
		t.Errorf("StopApp reached the substrate %v times on a sandbox with no app", l.provider.stopApps)
	}
}

func TestStopAppPassesTheForce(t *testing.T) {
	svc, l := newService(t, &recorder{}, withApp())

	if err := svc.StopApp(t.Context(), "sandbox1", false); err != nil {
		t.Fatalf("StopApp: %v", err)
	}
	if err := svc.StopApp(t.Context(), "sandbox1", true); err != nil {
		t.Fatalf("StopApp with force: %v", err)
	}
	if want := []bool{false, true}; !slices.Equal(l.provider.stopApps, want) {
		t.Errorf("the substrate got stops %v, want %v", l.provider.stopApps, want)
	}
}

// The app already ended, so the stop has nothing left to do and says so; the run's Ctrl+C reads it as done.
func TestStopAppRefusesAnAppThatEnded(t *testing.T) {
	svc, l := newService(t, &recorder{}, withApp())
	l.provider.restarts = models.RestartCount{Ended: true}

	err := svc.StopApp(t.Context(), "sandbox1", false)
	if got := appCode(err); got != models.CodeAppEnded {
		t.Fatalf("StopApp returned %v, want %s", err, models.CodeAppEnded)
	}
	if len(l.provider.stopApps) != 0 {
		t.Errorf("StopApp reached the substrate on an ended app: %v", l.provider.stopApps)
	}
}

func TestTheAppVerbsRefuseAStoppedSandbox(t *testing.T) {
	sb := withApp()
	sb.State = models.StateStopped
	svc, _ := newService(t, &recorder{}, sb)

	_, err := svc.WaitApp(t.Context(), "sandbox1")
	if got := appCode(err); got != models.CodeSandboxNotRunning {
		t.Errorf("WaitApp returned %v, want %s", err, models.CodeSandboxNotRunning)
	}
}

// A stop that ends the sandbox under a run would leave the wait polling for an end that never comes.
func TestWaitAppEndsWhenTheSandboxStopsFirst(t *testing.T) {
	svc, l := newService(t, &recorder{}, withApp())
	l.provider.appEnds = func() { l.provider.status = models.Status{Exists: true, State: models.StateStopped} }

	_, err := svc.WaitApp(t.Context(), "sandbox1")
	if got := appCode(err); got != models.CodeSandboxNotRunning {
		t.Errorf("WaitApp returned %v, want %s", err, models.CodeSandboxNotRunning)
	}
}
