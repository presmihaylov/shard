package sandbox_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func alpine() sandbox.CreateRequest {
	return sandbox.CreateRequest{Image: "alpine:3.20", Command: []string{"echo", "1"}}
}

func TestCreateAnswersTheRecordAndTearsNothingDown(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	sb, err := svc.Create(t.Context(), alpine())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if sb.ID != "sandbox1" || sb.State != models.StateRunning {
		t.Errorf("create answered %+v, want sandbox1 running", sb)
	}

	for _, call := range r.calls {
		if call == "repo.Delete" || call == "net.Release" || call == "provider.Remove" {
			t.Errorf("a successful create tore down %s; the sandbox outlives the verb", call)
		}
	}
}

// Create reports whether the create succeeded, and it never waits for a process a sandbox may outlive.
func TestCreateNeverWaitsForTheEntrypoint(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if slices.Contains(r.calls, "provider.Wait") {
		t.Errorf("create waited for the entrypoint: %v", r.calls)
	}
}

// TestCreateTearsDownWhatItBuilt forces a failure at each claim and asserts exactly what is given back.
func TestCreateTearsDownWhatItBuilt(t *testing.T) {
	cases := []struct {
		failAt  string
		cleanup []string
	}{
		{"images.Hold", nil},
		{"images.Pull", nil},
		{"repo.Create", nil},
		{"repo.Dir", []string{"repo.Delete"}},
		{"net.Allocate", []string{"net.Release", "repo.Delete"}},
		// Create rolls back its own mount only, so the substrate claim is given back here too.
		{"provider.Create", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"provider.Status", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"repo.Update#1", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"provider.Start", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		// The second update comes after a successful start, and a live sandbox is never given back.
		{"repo.Update#2", nil},
	}

	for _, c := range cases {
		t.Run(c.failAt, func(t *testing.T) {
			r := &recorder{fail: []string{c.failAt}}
			svc, _ := newService(t, r, models.Sandbox{})

			if _, err := svc.Create(t.Context(), alpine()); err == nil {
				t.Fatal("a forced failure returned no error")
			}

			if got := r.calls[len(r.calls)-len(c.cleanup):]; !slices.Equal(got, c.cleanup) {
				t.Errorf("tore down %v, want %v", got, c.cleanup)
			}

			// An empty tail matches anything, so the cases that give nothing back need their own assertion.
			if len(c.cleanup) > 0 {
				return
			}

			for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
				if slices.Contains(r.calls, step) {
					t.Errorf("tore down %v, want nothing", r.calls)
				}
			}
		})
	}
}

// A cancelled context would fail every give-back at once, so the unwind builds its own.
func TestCreateTearsDownAfterAnInterrupt(t *testing.T) {
	r := &recorder{fail: []string{"provider.Create"}}
	svc, _ := newService(t, r, models.Sandbox{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := svc.Create(ctx, alpine()); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, step := range []string{"provider.Remove", "net.Release"} {
		if !r.live[step] {
			t.Errorf("%s ran on the cancelled context, so it could not give anything back", step)
		}
	}
}

// The record write after a successful start is the one place the keep-alive rule nearly broke.
func TestCreateKeepsALiveSandboxWhenTheRecordWriteFails(t *testing.T) {
	r := &recorder{fail: []string{"repo.Update#2"}}
	svc, _ := newService(t, r, models.Sandbox{})

	_, err := svc.Create(t.Context(), alpine())
	if err == nil || !strings.Contains(err.Error(), "sandbox sandbox1 is running") {
		t.Fatalf("create = %v, want the live sandbox named", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s on a started sandbox; only stop ends one", step)
		}
	}
}

// The stack is LIFO, so a step that failed still holds what the steps below it name.
func TestCreateStopsUnwindingAtTheFirstFailure(t *testing.T) {
	r := &recorder{fail: []string{"provider.Start", "provider.Remove"}}
	svc, _ := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	if last := r.calls[len(r.calls)-1]; last != "provider.Remove" {
		t.Fatalf("the unwind went on to %q after provider.Remove failed", last)
	}

	for _, step := range []string{"net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after a failed give-back; the sandbox is still on the host", step)
		}
	}
}

// runsc reports the cancellation, not what it did, so a force-delete here could end a live guest.
func TestCreateKeepsASandboxAnInterruptedStartMayHaveStarted(t *testing.T) {
	r := &recorder{fail: []string{"provider.Start"}}
	svc, _ := newService(t, r, models.Sandbox{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := svc.Create(ctx, alpine())
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Fatalf("create = %v, want the kept sandbox named", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after an interrupted start; the entrypoint may already be live", step)
		}
	}
}

// A later process reaches the sandbox through the record alone, so what the substrate decided lands in it.
func TestCreateRecordsWhatTheSubstrateDecided(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"repo.Update#2"}}, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	if l.repo.sb.PID != 42 {
		t.Errorf("the record holds pid %d, want the one the substrate reported", l.repo.sb.PID)
	}
	if l.repo.sb.HostInterface != "shardv2" {
		t.Errorf("the record holds host interface %q, want shardv2", l.repo.sb.HostInterface)
	}
	if l.repo.sb.State != models.StateCreated {
		t.Errorf("the record says %s before the start was recorded, want created", l.repo.sb.State)
	}
}

// The pool is the one thing nothing frees on a timer, so its refusal names the verbs that do.
func TestCreateNamesLsWhenNoAddressIsFree(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})
	l.net.allocateErr = network.ErrNoFreeAddress

	_, err := svc.Create(t.Context(), alpine())
	if !errors.Is(err, network.ErrNoFreeAddress) || !strings.Contains(err.Error(), "shard ls --all") {
		t.Errorf("create failed with %v, want the pool's refusal naming shard ls --all", err)
	}
}

func TestCreateRefusesWhatNoStoreCouldHold(t *testing.T) {
	cases := map[string]sandbox.CreateRequest{
		"no image":                {},
		"a name no verb takes":    {Image: "alpine", Name: "a/b"},
		"a negative memory":       {Image: "alpine", Resources: models.Resources{MemoryMiB: -512}},
		"a memory that overflows": {Image: "alpine", Resources: models.Resources{MemoryMiB: sandbox.MaxMemoryMiB + 1}},
		"a negative cpu bound":    {Image: "alpine", Resources: models.Resources{VCPUs: -2}},
		"a bad policy name":       {Image: "alpine", Policy: "Bad Name"},
		"an env with no value":    {Image: "alpine", Env: []string{"DEBUG"}},
		"an env with no name":     {Image: "alpine", Env: []string{"=1"}},
		"a bad secret name":       {Image: "alpine", Secrets: []string{"api_key"}},
		"a doubled secret":        {Image: "alpine", Secrets: []string{"KEY", "KEY"}},
		"a secret an env shadows": {Image: "alpine", Secrets: []string{"KEY"}, Env: []string{"KEY=1"}},
	}

	for name, req := range cases {
		r := &recorder{}
		svc, _ := newService(t, r, models.Sandbox{})

		_, err := svc.Create(t.Context(), req)
		if err == nil {
			t.Errorf("create(%s) returned no error", name)
		}
		if len(r.calls) > 0 {
			t.Errorf("create(%s) reached a layer before the refusal: %v", name, r.calls)
		}
	}
}

func TestCreateHandsTheGuestThePlaceholderAndRecordsTheGrant(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})

	if _, err := l.secrets.Set("API_KEY", "sk-live-1234567890", []string{"api.example.com"}, ""); err != nil {
		t.Fatal(err)
	}

	req := alpine()
	req.Secrets = []string{"API_KEY"}
	req.Env = []string{"OTHER=1"}

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if !slices.Contains(l.provider.spec.Env, "API_KEY=mock-API_KEY") {
		t.Errorf("the guest env %v holds no placeholder", l.provider.spec.Env)
	}
	if slices.ContainsFunc(l.provider.spec.Env, func(e string) bool { return strings.Contains(e, "sk-live") }) {
		t.Fatalf("the guest env %v holds the value", l.provider.spec.Env)
	}

	if strings.Join(l.repo.created.Secrets, ",") != "API_KEY" {
		t.Errorf("the record grants %v, want API_KEY", l.repo.created.Secrets)
	}
}

func TestCreateRefusesASecretTheStoreDoesNotHoldBeforeThePull(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	req := alpine()
	req.Secrets = []string{"NOPE"}

	_, err := svc.Create(t.Context(), req)

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "secret NOPE does not exist") {
		t.Fatalf("create = %v, want a request error naming the secret", err)
	}
	if slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a missing secret still cost a pull: %v", r.calls)
	}
}

func TestCreateWithAPolicyTellsTheHostBeforeTheStart(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})

	if err := l.policies.Set(models.Policy{Name: "locked"}); err != nil {
		t.Fatal(err)
	}

	req := alpine()
	req.Policy = "locked"

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	want := []string{"net.Allocate", "provider.Create", "net.Reapply", "provider.Start"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}
	if got := l.repo.created.Policy; got != "locked" {
		t.Errorf("the record names policy %q, want locked", got)
	}
}

func TestCreateRefusesAPolicyTheStoreDoesNotHoldBeforeThePull(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	req := alpine()
	req.Policy = "ghost"

	_, err := svc.Create(t.Context(), req)

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "policy not found") {
		t.Fatalf("create = %v, want a request error naming the policy", err)
	}
	if slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a missing policy still cost a pull: %v", r.calls)
	}
}

func TestCreateWithoutAPolicyNeverReappliesTheRules(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if slices.Contains(r.calls, "net.Reapply") {
		t.Errorf("a create with no policy reapplied the rules: %v", r.calls)
	}
}

// The record carries the name so a later verb resolves it, and the spec so the guest hostname is it.
func TestCreateGivesTheNameToTheRecordAndToTheGuest(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})

	req := alpine()
	req.Name = "builder"

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := l.repo.created.Name; got != "builder" {
		t.Fatalf("the record carries the name %q, want builder", got)
	}
	if got := l.provider.spec.Name; got != "builder" {
		t.Fatalf("the spec carries the name %q, want builder", got)
	}
}

func TestCreateWithoutANameGivesTheGuestTheID(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})

	if _, err := svc.Create(t.Context(), alpine()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := l.repo.created.Name; got != "" {
		t.Fatalf("an unnamed create put %q in the record", got)
	}
	if l.provider.spec.Name != l.provider.spec.ID {
		t.Fatalf("the guest hostname is %q, want the id %q", l.provider.spec.Name, l.provider.spec.ID)
	}
}

func TestStartRunsAStoppedSandboxAgain(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, stopped())

	sb, err := svc.Start(t.Context(), "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if sb.ID != "sandbox1" {
		t.Errorf("start answered %q, want the id", sb.ID)
	}
	if !l.provider.started {
		t.Error("the provider was never asked to start")
	}

	// gVisor took the guest address at the first create, so the netns is built again before the run.
	if !l.net.allocated || slices.Index(r.calls, "net.Allocate") > slices.Index(r.calls, "provider.Start") {
		t.Errorf("the network was not built again before the start: %v", r.calls)
	}

	if sb.State != models.StateRunning || sb.PID != 7 {
		t.Errorf("the record is %s with pid %d, want running with pid 7", sb.State, sb.PID)
	}
	if sb.ExitStatus != nil {
		t.Errorf("the record still holds the old exit %+v", *sb.ExitStatus)
	}

	// The record says running only once the sandbox is, so a failed start leaves it stopped.
	if slices.Index(r.calls, "provider.Start") > slices.Index(r.calls, "repo.Update") {
		t.Errorf("the record was updated before the start: %v", r.calls)
	}
}

func TestStartRefusesASandboxThatIsNotStopped(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateCreated} {
		sb := running()
		sb.State = state
		svc, l := newService(t, &recorder{}, sb)

		_, err := svc.Start(t.Context(), "sandbox1")

		var refused *sandbox.StateError
		if !errors.As(err, &refused) || refused.State != state {
			t.Errorf("start of a %s sandbox returned %v, want a state error naming the state", state, err)
		}
		if l.provider.started {
			t.Errorf("start of a %s sandbox reached the provider", state)
		}
	}
}

func TestStartKeepsTheRecordStoppedWhenTheProviderFails(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Start"}}, stopped())

	if _, err := svc.Start(t.Context(), "sandbox1"); err == nil {
		t.Fatal("start returned no error")
	}

	if sb := l.repo.sb; sb.State != models.StateStopped || sb.ExitStatus == nil {
		t.Errorf("the record is %s with exit %v after a failed start, want stopped with its exit kept", sb.State, sb.ExitStatus)
	}
}

// A start that failed after the substrate came up leaves a live sandbox, and only stop ends one.
func TestStartRecordsASandboxThatCameUpUnderAFailedStart(t *testing.T) {
	svc, l := newService(t, &recorder{fail: []string{"provider.Start"}}, stopped())
	l.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	_, err := svc.Start(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "may be running") {
		t.Fatalf("start returned %v, want the failure and the warning that the sandbox stays", err)
	}

	if sb := l.repo.sb; sb.State != models.StateRunning || sb.PID != 9 {
		t.Errorf("the record is %s with pid %d, want running with pid 9", sb.State, sb.PID)
	}
}

func TestStartNamesTheSandboxWhenTheRecordWriteFails(t *testing.T) {
	svc, _ := newService(t, &recorder{fail: []string{"repo.Update"}}, stopped())

	_, err := svc.Start(t.Context(), "sandbox1")
	if err == nil || !strings.Contains(err.Error(), "is running but its record was not updated") {
		t.Errorf("start returned %v, want the sandbox named as running", err)
	}
}

func TestStartOfAMissingSandboxIsNotFound(t *testing.T) {
	svc, l := newService(t, &recorder{}, stopped())
	l.repo.missing = true

	if _, err := svc.Start(t.Context(), "sandbox1"); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Errorf("start = %v, want not found", err)
	}
}

// Two verbs on one sandbox run one after the other: a stop that arrives during a start waits for it.
func TestTheVerbsOnOneSandboxAreSerialized(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, stopped())

	gate := make(chan struct{})
	l.provider.gate = gate
	l.provider.entered = make(chan struct{})
	entered := l.provider.entered

	started := make(chan error, 1)
	go func() {
		_, err := svc.Start(t.Context(), "sandbox1")
		started <- err
	}()

	<-entered

	stopped := make(chan error, 1)
	go func() {
		_, err := svc.Stop(t.Context(), "sandbox1", time.Second)
		stopped <- err
	}()

	// The stop has no way to signal that it is waiting, so give it a moment to reach the lock.
	time.Sleep(20 * time.Millisecond)
	if slices.Contains(r.calls, "provider.Stop") {
		t.Fatal("the stop ran while the start held the sandbox")
	}

	close(gate)

	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}

	if !l.provider.stopped || l.repo.sb.State != models.StateStopped {
		t.Errorf("the stop that waited did not end the sandbox: %v", r.calls)
	}
}

// A stop ends the processes and keeps the record, the lease, the address and the writable layer.
func TestStopKeepsWhatOnlyRmFrees(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	sb, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}

	if !l.provider.stopped {
		t.Errorf("stop never reached the provider: %v", r.calls)
	}
	if l.provider.removed {
		t.Error("stop removed the sandbox from the substrate, which frees the writable layer")
	}
	if l.net.released {
		t.Error("stop released the address, which a start would then not get back")
	}
	if l.repo.deleted {
		t.Error("stop deleted the record, which is the only handle a start has")
	}
	if sb.ID != "sandbox1" || sb.State != models.StateStopped {
		t.Errorf("stop answered %+v, want sandbox1 stopped", sb)
	}
}

func TestStopRecordsTheExitStatus(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.exit = models.ExitStatus{Code: 143, Signal: 15}

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sb := l.repo.sb
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
	svc, l := newService(t, &recorder{}, running())
	l.provider.waitErr = models.ErrNoExitStatus

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("stop: %v", err)
	}

	sb := l.repo.sb
	if sb.State != models.StateStopped {
		t.Errorf("the record says %q, want stopped", sb.State)
	}
	if sb.ExitStatus != nil {
		t.Errorf("the record holds the exit status %+v of an entrypoint that never reported one", sb.ExitStatus)
	}
}

// A second stop must not overwrite the exit status the first one recorded.
func TestStopIsIdempotent(t *testing.T) {
	sb := running()
	sb.State = models.StateStopped
	sb.ExitStatus = &models.ExitStatus{Code: 143, Signal: 15}

	r := &recorder{}
	svc, l := newService(t, r, sb)

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("the second stop: %v", err)
	}

	if slices.Contains(r.calls, "provider.Stop") || slices.Contains(r.calls, "repo.Update") {
		t.Errorf("the second stop changed something: %v", r.calls)
	}
	if got := l.repo.sb.ExitStatus; got == nil || got.Signal != 15 {
		t.Errorf("the record holds %+v, want the exit status the first stop recorded", got)
	}
}

func TestStopRefusesAnIDThatNeverExisted(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.missing = true

	_, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace)
	if !errors.Is(err, sandboxstate.ErrNotFound) || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("stop failed with %v, want not found naming the id", err)
	}
	if slices.Contains(r.calls, "provider.Stop") {
		t.Errorf("stop reached the substrate for an id that never existed: %v", r.calls)
	}
}

func TestStopPassesTheGraceToTheProvider(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())

	if _, err := svc.Stop(t.Context(), "sandbox1", 3*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := l.provider.grace; got != 3*time.Second {
		t.Errorf("the provider got the grace %s, want 3s", got)
	}
}

// A record write that fails must not report success: the sandbox is gone and nothing says so.
func TestStopReportsARecordWriteThatFailed(t *testing.T) {
	svc, _ := newService(t, &recorder{fail: []string{"repo.Update"}}, running())

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err == nil {
		t.Fatal("a forced failure returned no error")
	}
}

func TestStopReportsAWaitThatFailedForAnotherReason(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.waitErr = errors.New("the exit status was unreadable")

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err == nil {
		t.Fatal("an unreadable exit status returned no error")
	}
}

// A start that failed after the substrate came up leaves a stopped record over a live sandbox.
func TestStopEndsASandboxWhoseRecordSaysStopped(t *testing.T) {
	sb := running()
	sb.State = models.StateStopped

	r := &recorder{}
	svc, l := newService(t, r, sb)
	l.provider.status = models.Status{Exists: true, State: models.StateRunning, PID: 9}

	if _, err := svc.Stop(t.Context(), "sandbox1", sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !l.provider.stopped {
		t.Errorf("the live sandbox was not stopped: %v", r.calls)
	}
}

func stoppedOnTheHost(t *testing.T, r *recorder) (*sandbox.Service, layers) {
	t.Helper()

	sb := running()
	sb.State = models.StateStopped

	return newService(t, r, sb)
}

// The record dies last, because it is the only handle by which the mount and the namespace are found.
func TestRemoveFreesEveryHoldingInOrder(t *testing.T) {
	r := &recorder{}
	svc, _ := stoppedOnTheHost(t, r)

	if err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("rm: %v", err)
	}

	want := []string{"provider.Remove", "net.Release", "repo.Delete", "repo.List", "substrate.DropNullNetns"}
	if got := r.calls[len(r.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("rm freed %v, want %v", got, want)
	}
}

// runsc bind mounts a null-netns into its own root and never drops it, so the last rm does.
func TestRemoveOfTheLastSandboxDropsWhatTheSubstrateKeeps(t *testing.T) {
	r := &recorder{}
	svc, l := stoppedOnTheHost(t, r)

	if err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if !l.substrate.dropped {
		t.Errorf("the rm of the last sandbox left the substrate mount: %v", r.calls)
	}
}

func TestRemoveKeepsWhatTheSubstrateSharesWhileASandboxIsLeft(t *testing.T) {
	svc, l := stoppedOnTheHost(t, &recorder{})
	l.repo.left = []models.Sandbox{{ID: "sandbox2"}}

	if err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if l.substrate.dropped {
		t.Error("the rm dropped the substrate mount while another sandbox still uses the root")
	}
}

func TestRemoveRefusesARunningSandbox(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running())

	err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace)

	var refused *sandbox.StateError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "shard stop sandbox1") {
		t.Fatalf("rm failed with %v, want a state error that says to stop the sandbox first", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("the refused rm still freed %s: %v", step, r.calls)
		}
	}
}

// force is the shorthand for the stop the operator would type first.
func TestRemoveForceStopsThenRemoves(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())

	if err := svc.Remove(t.Context(), "sandbox1", true, 3*time.Second); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	if !l.provider.stopped || !l.provider.removed {
		t.Errorf("rm --force stopped=%v removed=%v, want both", l.provider.stopped, l.provider.removed)
	}
	if l.provider.grace != 3*time.Second {
		t.Errorf("the stop under rm --force got the grace %s, want 3s", l.provider.grace)
	}

	stopped := slices.Index(r.calls, "provider.Stop")
	removed := slices.Index(r.calls, "provider.Remove")
	if stopped < 0 || removed < stopped {
		t.Errorf("rm --force ran %v, want the stop before the remove", r.calls)
	}
}

// The record dies last, so an id with no record has nothing else left either.
func TestRemoveOfAMissingSandboxIsNotFound(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	l.repo.missing = true

	if err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace); !errors.Is(err, sandboxstate.ErrNotFound) {
		t.Fatalf("rm of an id that is already gone = %v, want not found", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("rm of an id that is already gone freed %s: %v", step, r.calls)
		}
	}
}

// A partial rm must be legible, so the error lists every holding from the one that failed onwards.
func TestRemoveNamesWhatIsLeftOnTheHost(t *testing.T) {
	svc, l := stoppedOnTheHost(t, &recorder{fail: []string{"net.Release"}})

	err := svc.Remove(t.Context(), "sandbox1", false, sandbox.DefaultStopGrace)
	if err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, left := range []string{"netns, veth and address lease", "record and state directory"} {
		if !strings.Contains(err.Error(), left) {
			t.Errorf("rm failed with %v, want it to name the %s it left behind", err, left)
		}
	}
	if strings.Contains(err.Error(), "runsc state and rootfs mount") {
		t.Errorf("rm failed with %v, but it did free the runsc state", err)
	}
	if l.repo.deleted {
		t.Error("rm deleted the record past a failure, so nothing can reach what is left")
	}
}
