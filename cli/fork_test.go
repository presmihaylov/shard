package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestForkStartsANewSandboxFromTheSnapshot(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	source := paused()
	source.Image = "docker.io/library/alpine:3.20"
	source.Resources = models.Resources{MemoryMiB: 256}
	source.ExitStatus = &models.ExitStatus{Code: 3}
	app, d := newLifecycleApp(t, &out, r, source)

	if err := app.Run(t.Context(), []string{"fork", "--name", "web-2", "web"}); err != nil {
		t.Fatalf("fork: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox2" {
		t.Errorf("fork printed %q, want the new id", got)
	}

	provider := d.providerSvc.(*fakeLifecycleProvider)
	if provider.snapshot != "/snapshots/sandbox1" {
		t.Errorf("the provider was told to read %q, want the source's snapshot", provider.snapshot)
	}
	if provider.forked.ID != "sandbox2" || provider.forked.Name != "web-2" || provider.forked.StateDir != "/state/sandbox2" {
		t.Errorf("the provider was handed %+v, want the fork's own id, name and state directory", provider.forked)
	}
	if provider.forked.Resources != source.Resources {
		t.Errorf("the fork was bound to %+v, want the source's %+v", provider.forked.Resources, source.Resources)
	}

	// The fork has its own netns, and the host rules go on again once the guest is up in it.
	want := []string{"net.Allocate", "provider.Fork", "net.Reapply"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}

	repo := d.repoSvc.(*fakeLifecycleRepo)
	if repo.created.Image != source.Image || repo.created.Name != "web-2" || repo.created.Resources != source.Resources {
		t.Errorf("the record was created as %+v, want the source's image and bound under the new name", repo.created)
	}
	if repo.created.State != models.StateRunning || repo.created.PID != 9 {
		t.Errorf("the fork's record is %s with pid %d, want running with pid 9", repo.created.State, repo.created.PID)
	}
	if repo.created.NetnsPath != "/run/netns/sandbox2" || repo.created.HostInterface != "veth-sandbox2" {
		t.Errorf("the fork's record holds the network %+v, want its own netns and interface", repo.created)
	}
	if repo.created.ExitStatus == nil || repo.created.ExitStatus.Code != 3 {
		t.Errorf("the fork's record holds the exit %+v, want the source's, which its memory image carries", repo.created.ExitStatus)
	}

	// A fork only reads the source, so other forks may run beside it and only a pause or an rm waits.
	if !slices.Contains(r.calls, "repo.HoldShared") || slices.Contains(r.calls, "repo.Hold") {
		t.Errorf("the source was held as %v, want a shared hold", keep(r.calls, "repo.Hold", "repo.HoldShared"))
	}

	// The source is read and nothing more: its record and its snapshot are as they were.
	if repo.sb.State != models.StatePaused || repo.sb.Snapshot != "/snapshots/sandbox1" {
		t.Errorf("the source's record changed to %+v", repo.sb)
	}
	if repo.deleted {
		t.Error("a fork that succeeded deleted a record")
	}
}

// A fork reads the snapshot, so the source may be in any state that has one, the running one included.
func TestForkTakesASourceInAnyStateThatHasASnapshot(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateStopped, models.StatePaused} {
		sb := paused()
		sb.State = state
		app, _ := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, sb)

		if err := app.Run(t.Context(), []string{"fork", "sandbox1"}); err != nil {
			t.Errorf("fork of a %s sandbox: %v", state, err)
		}
	}
}

func TestForkRefusesASourceWithNoSnapshot(t *testing.T) {
	app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, running())

	err := app.Run(t.Context(), []string{"fork", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Errorf("fork of a sandbox that was never paused returned %v, want a refusal that says so", err)
	}
	if slices.Contains(d.repoSvc.(*fakeLifecycleRepo).r.calls, "repo.Create") {
		t.Error("the refusal came after a record was created")
	}
}

func TestForkRefusesABadName(t *testing.T) {
	app, _ := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, paused())

	if err := app.Run(t.Context(), []string{"fork", "--name", "", "sandbox1"}); err == nil {
		t.Error("fork accepted an empty name")
	}
}

// Everything claimed before the restore goes back when it fails, and the source is left alone.
func TestForkGivesBackWhatItClaimedWhenTheRestoreFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Fork"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, paused())

	if err := app.Run(t.Context(), []string{"fork", "sandbox1"}); err == nil {
		t.Fatal("fork reported success when the restore failed")
	}

	want := []string{"provider.Fork", "provider.Remove", "net.Release", "repo.Delete"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the teardown ran as %v, want %v", got, want)
	}

	repo := d.repoSvc.(*fakeLifecycleRepo)
	if repo.deletedID != "sandbox2" {
		t.Errorf("the teardown deleted %q, want the fork's record and never the source's", repo.deletedID)
	}
	if repo.sb.State != models.StatePaused {
		t.Errorf("the source's record changed to %s", repo.sb.State)
	}
}

// The fork is live once the restore returns, so a failure after it keeps the sandbox and its record.
func TestForkKeepsTheSandboxWhenTheRulesFail(t *testing.T) {
	r := &recorder{fail: []string{"net.Reapply"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, paused())

	if err := app.Run(t.Context(), []string{"fork", "sandbox1"}); err == nil {
		t.Fatal("fork reported success when the rules failed")
	}

	if slices.Contains(r.calls, "provider.Remove") || slices.Contains(r.calls, "repo.Delete") {
		t.Errorf("a live fork was torn down: %v", r.calls)
	}
	got := d.repoSvc.(*fakeLifecycleRepo).created
	if got.State != models.StateRunning || got.PID != 9 {
		t.Errorf("the fork's record is %s with pid %d, want running with its pid", got.State, got.PID)
	}
	// The record must name the netns and the interface, or a later rm cannot give them back.
	if got.NetnsPath != "/run/netns/sandbox2" || got.Address.String() != "10.0.0.2/24" || got.HostInterface != "veth-sandbox2" {
		t.Errorf("the fork's record holds the network %+v, want the one it was allocated", got)
	}
}

// keep filters the calls down to the named ones, in the order they happened.
func keep(calls []string, names ...string) []string {
	var got []string
	for _, c := range calls {
		if slices.Contains(names, c) {
			got = append(got, c)
		}
	}

	return got
}

func TestForkCarriesThePolicyAndTellsTheHostBeforeTheRestore(t *testing.T) {
	r := &recorder{}
	source := paused()
	source.Policy = "locked"
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, source)

	if err := app.Run(t.Context(), []string{"fork", "sandbox1"}); err != nil {
		t.Fatalf("fork: %v", err)
	}

	want := []string{"net.Allocate", "net.Reapply", "provider.Fork", "net.Reapply"}
	if got := keep(r.calls, "net.Allocate", "net.Reapply", "provider.Fork"); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}
	if got := d.repoSvc.(*fakeLifecycleRepo).created.Policy; got != "locked" {
		t.Errorf("the fork names policy %q, want the source's", got)
	}
}

func TestForkOfAFrontedSandboxNeedsTheDaemon(t *testing.T) {
	r := &recorder{}
	source := paused()
	source.Secrets = []string{"KEY"}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, source)
	d.aliveFn = daemonDown

	if err := app.Run(t.Context(), []string{"fork", "sandbox1"}); !errors.Is(err, errDaemonDown) {
		t.Errorf("fork = %v, want the daemon refusal", err)
	}
	if slices.Contains(r.calls, "repo.Create") {
		t.Errorf("a refused fork still claimed a record: %v", r.calls)
	}
}
