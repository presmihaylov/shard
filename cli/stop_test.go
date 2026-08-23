package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

func TestParseStopFlags(t *testing.T) {
	opts, err := parseStop([]string{"--time", "45s", "sandbox1"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if opts.id != "sandbox1" || opts.grace != 45*time.Second {
		t.Errorf("parseStop gave %+v, want sandbox1 and 45s", opts)
	}
}

func TestParseStopDefaultsTheGrace(t *testing.T) {
	opts, err := parseStop([]string{"sandbox1"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if opts.grace != DefaultStopGrace {
		t.Errorf("the grace is %s, want %s", opts.grace, DefaultStopGrace)
	}
}

func TestParseStopRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":            {},
		"two ids":          {"sandbox1", "sandbox2"},
		"a flag after id":  {"sandbox1", "--time", "5s"},
		"a negative grace": {"--time", "-5s", "sandbox1"},
		"an unknown flag":  {"--forever", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseStop(args); err == nil {
			t.Errorf("parseStop(%s) returned no error", name)
		}
	}
}

// TestStopKeepsWhatOnlyRmFrees is the boundary between the two verbs: a stop ends the processes and
// keeps the record, the lease, the address and the writable layer, so SHARD-96 can start it again.
func TestStopKeepsWhatOnlyRmFrees(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	provider := deps.providerSvc.(*fakeLifecycleProvider)
	if !provider.stopped {
		t.Errorf("stop never reached the provider: %v", r.calls)
	}
	if provider.removed {
		t.Error("stop removed the sandbox from the substrate, which frees the writable layer")
	}
	if deps.netSvc.(*fakeLifecycleNet).released {
		t.Error("stop released the address, which a start would then not get back")
	}
	if deps.repoSvc.(*fakeLifecycleRepo).deleted {
		t.Error("stop deleted the record, which is the only handle a start has")
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("stop printed %q, want the bare id", out.String())
	}
}

func TestStopRecordsTheExitStatus(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.providerSvc.(*fakeLifecycleProvider).exit = models.ExitStatus{Code: 143, Signal: 15}

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sb := deps.repoSvc.(*fakeLifecycleRepo).sb
	if sb.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", sb.State)
	}
	if sb.PID != 0 {
		t.Errorf("the record still names the host pid %d of a sandbox that is gone", sb.PID)
	}
	if sb.ExitStatus == nil || sb.ExitStatus.Signal != 15 {
		t.Errorf("the record holds the exit status %+v, want the SIGTERM the stop sent", sb.ExitStatus)
	}
}

// A stop that had to kill leaves no exit status: the supervisor died before it could record one.
func TestStopRecordsNoExitStatusWhenTheSandboxWasKilled(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.providerSvc.(*fakeLifecycleProvider).waitErr = models.ErrNoExitStatus

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sb := deps.repoSvc.(*fakeLifecycleRepo).sb
	if sb.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", sb.State)
	}
	if sb.ExitStatus != nil {
		t.Errorf("the record holds the exit status %+v of an entrypoint that never reported one", sb.ExitStatus)
	}
}

// TestStopIsIdempotent: a second stop must not overwrite the exit status the first one recorded.
func TestStopIsIdempotent(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped
	sb.ExitStatus = &models.ExitStatus{Code: 143, Signal: 15}

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, sb)

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err != nil {
		t.Fatalf("the second stop: %v", err)
	}

	if slices.Contains(r.calls, "provider.Stop") || slices.Contains(r.calls, "repo.Update") {
		t.Errorf("the second stop changed something: %v", r.calls)
	}

	if got := deps.repoSvc.(*fakeLifecycleRepo).sb.ExitStatus; got == nil || got.Signal != 15 {
		t.Errorf("the record holds %+v, want the exit status the first stop recorded", got)
	}
}

func TestStopRefusesAnIDThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"stop", "sandbox1"})
	if err == nil {
		t.Fatal("stop of an id that never existed returned no error")
	}
	if !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("stop failed with %v, want it to name the id", err)
	}

	if slices.Contains(r.calls, "provider.Stop") {
		t.Errorf("stop reached the substrate for an id that never existed: %v", r.calls)
	}
}

func TestStopPassesTheGraceToTheProvider(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())

	if err := app.Run(t.Context(), []string{"stop", "--time", "3s", "sandbox1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := deps.providerSvc.(*fakeLifecycleProvider).grace; got != 3*time.Second {
		t.Errorf("the provider got the grace %s, want 3s", got)
	}
}

// A record write that fails must not report success: the sandbox is gone and nothing says so.
func TestStopReportsARecordWriteThatFailed(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"repo.Update"}}
	app, _ := newLifecycleApp(t, &out, r, running())

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err == nil {
		t.Fatal("a forced failure returned no error")
	}
}

func TestStopReportsAWaitThatFailedForAnotherReason(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.providerSvc.(*fakeLifecycleProvider).waitErr = errors.New("the exit status was unreadable")

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err == nil {
		t.Fatal("an unreadable exit status returned no error")
	}
}
