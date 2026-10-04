package sandbox_test

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

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

// The run client exits with the code and an inspect reads the record next, so the exit is on it before either verb answers (SHARD-479).
func TestTheAppVerbsRecordTheExitBeforeTheyAnswer(t *testing.T) {
	verbs := map[string]func(*testing.T, *sandbox.Service) error{
		"AttachApp": func(t *testing.T, svc *sandbox.Service) error {
			_, err := svc.AttachApp(t.Context(), "sandbox1", func() (io.Writer, error) { return io.Discard, nil })
			return err
		},
		"WaitApp": func(t *testing.T, svc *sandbox.Service) error {
			_, err := svc.WaitApp(t.Context(), "sandbox1")
			return err
		},
	}
	for verb, call := range verbs {
		t.Run(verb, func(t *testing.T) {
			svc, l, _ := logsOf(t, &recorder{}, withApp(), "")
			l.provider.entrypointExit = &models.ExitStatus{Code: 3}
			l.provider.restarts = models.RestartCount{Ended: true}

			if err := call(t, svc); err != nil {
				t.Fatalf("%s: %v", verb, err)
			}

			if got := l.repo.sb; got.State != models.StateRunning || got.ExitStatus == nil || *got.ExitStatus != (models.ExitStatus{Code: 3}) {
				t.Errorf("the record says %s with exit %+v, want running with {code:3}", got.State, got.ExitStatus)
			}
		})
	}
}

// The tick skips a sandbox a verb holds, but the run's own write waits for it, or the record misses the exit until a later tick.
func TestWaitAppRecordsTheExitOnceTheVerbHoldingTheSandboxLetsGo(t *testing.T) {
	svc, l := newService(t, &recorder{}, withApp())
	l.provider.entrypointExit = &models.ExitStatus{Code: 3}
	l.provider.restarts = models.RestartCount{Ended: true}
	unlock, err := svc.Hold(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	waited := make(chan error, 1)
	go func() {
		_, err := svc.WaitApp(t.Context(), "sandbox1")
		waited <- err
	}()
	waitForWaiters(t, svc, "sandbox1", 2)
	unlock()

	if err := <-waited; err != nil {
		t.Fatalf("WaitApp: %v", err)
	}
	if got := l.repo.sb.ExitStatus; got == nil || *got != (models.ExitStatus{Code: 3}) {
		t.Errorf("the record holds exit %+v, want {code:3}", got)
	}
}

// A verb that took the sandbox first changed the run, so the app's end belongs to no record and the record stays as that verb left it.
func TestWaitAppLeavesTheRecordOfAVerbThatLandedFirst(t *testing.T) {
	cases := map[string]func(*models.Sandbox, *fakeProvider){
		"a stop": func(sb *models.Sandbox, _ *fakeProvider) {
			sb.State = models.StateStopped
			sb.ExitStatus = &models.ExitStatus{Code: 143, Signal: 15}
		},
		"a stop and a start": func(sb *models.Sandbox, p *fakeProvider) {
			sb.StartedAt = time.Now().UTC()
			p.entrypointExit = nil
			p.restarts = models.RestartCount{}
		},
	}
	for name, land := range cases {
		t.Run(name, func(t *testing.T) {
			svc, l := newService(t, &recorder{}, withApp())
			l.provider.entrypointExit = &models.ExitStatus{Code: 3}
			l.provider.restarts = models.RestartCount{Ended: true}
			unlock, err := svc.Hold(t.Context(), "sandbox1")
			if err != nil {
				t.Fatalf("hold: %v", err)
			}

			waited := make(chan models.AppExit, 1)
			go func() {
				exit, err := svc.WaitApp(t.Context(), "sandbox1")
				if err != nil {
					t.Errorf("WaitApp: %v", err)
				}
				waited <- exit
			}()
			waitForWaiters(t, svc, "sandbox1", 2)
			land(&l.repo.sb, l.provider)
			want := l.repo.sb
			unlock()

			if exit := <-waited; exit != (models.AppExit{Code: 3}) {
				t.Errorf("WaitApp answered %+v, want the app's own {code:3}", exit)
			}
			if got := l.repo.sb; got.State != want.State || !reflect.DeepEqual(got.ExitStatus, want.ExitStatus) {
				t.Errorf("the record says %s with exit %+v, want %s with %+v as %s left it", got.State, got.ExitStatus, want.State, want.ExitStatus, name)
			}
		})
	}
}

// A resume starts the record's clock again but keeps the run, so the app's end still lands on it.
func TestWaitAppRecordsTheEndAcrossAResume(t *testing.T) {
	sb := withApp()
	sb.Restart = &models.Restart{RestartSpec: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 1}}
	svc, l := newService(t, &recorder{}, sb)
	l.provider.entrypointExit = &models.ExitStatus{Code: 3}
	l.provider.restarts = models.RestartCount{Count: 2, GaveUp: true, Ended: true}
	unlock, err := svc.Hold(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	waited := make(chan error, 1)
	go func() {
		_, err := svc.WaitApp(t.Context(), "sandbox1")
		waited <- err
	}()
	waitForWaiters(t, svc, "sandbox1", 2)
	l.repo.sb.StartedAt = time.Now().UTC()
	unlock()

	if err := <-waited; err != nil {
		t.Fatalf("WaitApp: %v", err)
	}
	got := l.repo.sb
	if got.ExitStatus == nil || *got.ExitStatus != (models.ExitStatus{Code: 3}) {
		t.Errorf("the record holds exit %+v, want {code:3}", got.ExitStatus)
	}
	if got.Restart == nil || got.Restart.RestartCount != l.provider.restarts {
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
