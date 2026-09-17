package sandbox_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// policied is a running sandbox under an on-failure policy that has not started again yet.
func policied() models.Sandbox {
	sb := running()
	sb.Restart = &models.Restart{RestartSpec: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 5, Backoff: 1}}

	return sb
}

func TestCreateHandsTheRestartPolicyToTheProviderWithTheDefaultsFilled(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})
	req := alpine()
	req.Restart = &models.RestartSpec{Policy: models.RestartAlways}

	sb, err := svc.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	want := &models.Restart{RestartSpec: models.RestartSpec{Policy: models.RestartAlways, Backoff: 1}}
	if !reflect.DeepEqual(sb.Restart, want) || !reflect.DeepEqual(l.repo.sb.Restart, want) {
		t.Errorf("the record holds the policy %+v, want %+v with the defaults filled and nothing counted", sb.Restart, want)
	}
	if l.provider.spec.Restart != want.RestartSpec {
		t.Errorf("the provider was handed the policy %+v, want %+v", l.provider.spec.Restart, want.RestartSpec)
	}
}

func TestCreateRecordsNoPolicyThatNeverStartsAgain(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})
	req := alpine()
	req.Restart = &models.RestartSpec{Policy: models.RestartNo}

	sb, err := svc.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if sb.Restart != nil || l.provider.spec.Restart.Set() {
		t.Errorf("the record holds %+v and the provider was handed %+v, want no policy on either", sb.Restart, l.provider.spec.Restart)
	}
}

func TestCreateRefusesARestartPolicyNoSupervisorTakes(t *testing.T) {
	cases := map[string]models.RestartSpec{
		"an unknown policy":      {Policy: "unless-stopped"},
		"no policy but settings": {Retries: 3},
		"never but settings":     {Policy: models.RestartNo, Backoff: 2},
		"always with retries":    {Policy: models.RestartAlways, Retries: 3},
		"negative retries":       {Policy: models.RestartOnFailure, Retries: -1},
		"negative backoff":       {Policy: models.RestartAlways, Backoff: -1},
		"a backoff past the cap": {Policy: models.RestartAlways, Backoff: models.RestartBackoffCap + 1},
	}

	for name, restart := range cases {
		r := &recorder{}
		svc, _ := newService(t, r, models.Sandbox{})
		req := alpine()
		req.Restart = &restart

		_, err := svc.Create(t.Context(), req)
		if err == nil {
			t.Errorf("create(%s) returned no error", name)
		}
		if len(r.calls) > 0 {
			t.Errorf("create(%s) reached a layer before the refusal: %v", name, r.calls)
		}
	}
}

func TestRecordRestartsCopiesWhatTheSupervisorCounted(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, policied())
	at := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	l.provider.restarts = models.RestartCount{Count: 2, LastAt: at}

	var reports []string
	report := func(line string) { reports = append(reports, line) }
	if err := svc.RecordRestarts(t.Context(), []models.Sandbox{policied()}, report); err != nil {
		t.Fatalf("RecordRestarts: %v", err)
	}

	if got := l.repo.sb.Restart.RestartCount; got.Count != 2 || !got.LastAt.Equal(at) || got.GaveUp {
		t.Errorf("the record counts %+v, want 2 starts again at %v", got, at)
	}
	if len(reports) != 1 || reports[0] != "sandbox sandbox1: the entrypoint was started again, 2 of 5" {
		t.Errorf("the tick reported %v, want one line for the starts again", reports)
	}

	// The same count again is not news, and the record is not written for it.
	reports = nil
	updates := len(keep(r.calls, "repo.Update"))
	if err := svc.RecordRestarts(t.Context(), []models.Sandbox{l.repo.sb}, report); err != nil {
		t.Fatalf("RecordRestarts again: %v", err)
	}
	if len(reports) != 0 || len(keep(r.calls, "repo.Update")) != updates {
		t.Errorf("a tick with nothing new reported %v and wrote the record", reports)
	}

	l.provider.restarts = models.RestartCount{Count: 5, LastAt: at.Add(time.Minute), GaveUp: true}
	if err := svc.RecordRestarts(t.Context(), []models.Sandbox{l.repo.sb}, report); err != nil {
		t.Fatalf("RecordRestarts at the give-up: %v", err)
	}
	want := []string{
		"sandbox sandbox1: the entrypoint was started again, 5 of 5",
		"sandbox sandbox1: the entrypoint exited again and the 5 starts again the policy allows are spent",
	}
	if !reflect.DeepEqual(reports, want) || !l.repo.sb.Restart.GaveUp {
		t.Errorf("the give-up reported %v and recorded %+v, want %v and the give-up", reports, l.repo.sb.Restart, want)
	}
}

func TestRecordRestartsWritesAFreshStartTheCountDoesNotShow(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	sb := policied()
	sb.Restart.RestartCount = models.RestartCount{Count: 1, LastAt: t1}
	svc, l := newService(t, &recorder{}, sb)

	t2 := t1.Add(time.Minute)
	l.provider.restarts = models.RestartCount{Count: 1, LastAt: t2}

	var reports []string
	report := func(line string) { reports = append(reports, line) }
	if err := svc.RecordRestarts(t.Context(), []models.Sandbox{sb}, report); err != nil {
		t.Fatalf("RecordRestarts: %v", err)
	}

	if got := l.repo.sb.Restart.RestartCount; got.Count != 1 || !got.LastAt.Equal(t2) {
		t.Errorf("the record counts %+v, want the same 1 start again stamped at the fresh %v", got, t2)
	}
	if len(reports) != 1 || reports[0] != "sandbox sandbox1: the entrypoint was started again, 1 of 5" {
		t.Errorf("a fresh start the count did not move reported %v, want one line for it", reports)
	}
}

func TestRecordRestartsLeavesARecordWithNoPolicyOrNotRunning(t *testing.T) {
	stopped := policied()
	stopped.State = models.StateStopped
	for name, sb := range map[string]models.Sandbox{"no policy": running(), "stopped": stopped} {
		r := &recorder{}
		svc, _ := newService(t, r, sb)

		if err := svc.RecordRestarts(t.Context(), []models.Sandbox{sb}, func(string) {}); err != nil {
			t.Fatalf("RecordRestarts(%s): %v", name, err)
		}
		if len(keep(r.calls, "provider.Restarts")) != 0 {
			t.Errorf("RecordRestarts(%s) asked the provider for a count nothing keeps", name)
		}
	}
}

func TestRecordRestartsReportsACountItCannotRead(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Restarts"}}, policied())

	err := svc.RecordRestarts(t.Context(), []models.Sandbox{policied()}, func(string) {})
	if err == nil {
		t.Fatal("a count that cannot be read returned no error")
	}
	if l.repo.sb.Restart.Count != 0 {
		t.Errorf("the record counts %+v after a failed read", l.repo.sb.Restart)
	}
}

func TestStopRecordsTheLastRestartCount(t *testing.T) {
	svc, l := newService(t, &recorder{}, policied())
	l.provider.restarts = models.RestartCount{Count: 3, GaveUp: true}

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := l.repo.sb.Restart; got.Count != 3 || !got.GaveUp || got.Policy != models.RestartOnFailure {
		t.Errorf("the stopped record holds %+v, want the policy with the 3 starts again and the give-up", got)
	}
}

func TestStartDropsTheCountOfTheLastRun(t *testing.T) {
	sb := policied()
	sb.State = models.StateStopped
	sb.Restart.RestartCount = models.RestartCount{Count: 5, GaveUp: true}
	svc, _ := newService(t, &recorder{}, sb)

	started, err := svc.Start(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	want := &models.Restart{RestartSpec: sb.Restart.RestartSpec}
	if !reflect.DeepEqual(started.Restart, want) {
		t.Errorf("the new run holds %+v, want the policy with nothing counted", started.Restart)
	}
}

func TestResumeKeepsTheCountOfTheRunItFroze(t *testing.T) {
	sb := pausedSandbox()
	sb.Restart = &models.Restart{
		RestartSpec:  models.RestartSpec{Policy: models.RestartAlways, Retries: 5, Backoff: 1},
		RestartCount: models.RestartCount{Count: 2},
	}
	svc, l := newService(t, &recorder{}, sb)
	l.provider.status = models.Status{}

	resumed, err := svc.Resume(t.Context(), "web")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if !reflect.DeepEqual(resumed.Restart, sb.Restart) {
		t.Errorf("the resumed record holds %+v, want the count the pause froze %+v", resumed.Restart, sb.Restart)
	}
}

func TestForkKeepsTheCountAndCloneStartsItOver(t *testing.T) {
	restart := &models.Restart{
		RestartSpec:  models.RestartSpec{Policy: models.RestartOnFailure, Retries: 5, Backoff: 1},
		RestartCount: models.RestartCount{Count: 2},
	}

	source := pausedSandbox()
	source.Restart = restart
	svc, _ := newService(t, &recorder{}, source)
	forked, err := svc.Fork(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !reflect.DeepEqual(forked.Restart, restart) {
		t.Errorf("the fork holds %+v, want the source's policy and count %+v", forked.Restart, restart)
	}

	source = cloneSource()
	source.Restart = restart
	svc, _ = newService(t, &recorder{}, source)
	cloned, err := svc.Clone(t.Context(), "web", sandbox.CopyRequest{Name: "web-2"})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if want := (&models.Restart{RestartSpec: restart.RestartSpec}); !reflect.DeepEqual(cloned.Restart, want) {
		t.Errorf("the clone holds %+v, want the source's policy with nothing counted", cloned.Restart)
	}
}
