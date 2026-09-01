package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// cloneSource is a stopped sandbox with the image and the bound a clone must carry over.
func cloneSource() models.Sandbox {
	sb := stopped()
	sb.Image = "docker.io/library/alpine:3.20"
	sb.Resources = models.Resources{MemoryMiB: 256}

	return sb
}

func TestCloneStartsANewSandboxOverTheSourcesFiles(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	source := cloneSource()
	app, d := newLifecycleApp(t, &out, r, source)

	if err := app.Run(t.Context(), []string{"clone", "--name", "web-2", "web"}); err != nil {
		t.Fatalf("clone: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox2" {
		t.Errorf("clone printed %q, want the new id", got)
	}

	provider := d.providerSvc.(*fakeLifecycleProvider)
	if provider.clonedFrom != "sandbox1" {
		t.Errorf("the provider was told to copy %q, want the source's id", provider.clonedFrom)
	}
	if provider.cloned.ID != "sandbox2" || provider.cloned.Name != "web-2" || provider.cloned.StateDir != "/state/sandbox2" {
		t.Errorf("the provider was handed %+v, want the clone's own id, name and state directory", provider.cloned)
	}
	if provider.cloned.Network.NetnsPath != "/run/netns/sandbox2" {
		t.Errorf("the provider was handed the network %+v, want the clone's own netns", provider.cloned.Network)
	}

	// The clone comes up in a fresh netns the allocation ruled, as a create does, so nothing is applied again.
	want := []string{"net.Allocate", "provider.Clone"}
	if got := keep(r.calls, "net.Allocate", "provider.Clone", "net.Reapply"); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}

	repo := d.repoSvc.(*fakeLifecycleRepo)
	if repo.created.Image != source.Image || repo.created.Name != "web-2" || repo.created.Resources != source.Resources {
		t.Errorf("the record was created as %+v, want the source's image and bound under the new name", repo.created)
	}
	if repo.created.State != models.StateRunning || repo.created.PID != 11 {
		t.Errorf("the clone's record is %s with pid %d, want running with pid 11", repo.created.State, repo.created.PID)
	}
	if repo.created.NetnsPath != "/run/netns/sandbox2" || repo.created.HostInterface != "veth-sandbox2" {
		t.Errorf("the clone's record holds the network %+v, want its own netns and interface", repo.created)
	}
	// The entrypoint runs from the beginning, so the source's exit says nothing about the clone.
	if repo.created.ExitStatus != nil {
		t.Errorf("the clone's record holds the exit %+v, want none", repo.created.ExitStatus)
	}
	if repo.created.Snapshot != "" {
		t.Errorf("the clone's record names the snapshot %q, want none", repo.created.Snapshot)
	}

	// A clone only reads the source, so other clones may run beside it and only a start, a stop or an rm waits.
	if !slices.Contains(r.calls, "repo.HoldShared") || slices.Contains(r.calls, "repo.Hold") {
		t.Errorf("the source was held as %v, want a shared hold", keep(r.calls, "repo.Hold", "repo.HoldShared"))
	}

	if repo.sb.State != models.StateStopped || repo.sb.ExitStatus == nil {
		t.Errorf("the source's record changed to %+v", repo.sb)
	}
	if repo.deleted {
		t.Error("a clone that succeeded deleted a record")
	}
}

// A paused sandbox holds its files as they were at the pause, and nothing writes them, so it clones too.
func TestCloneTakesAPausedSource(t *testing.T) {
	app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, paused())

	if err := app.Run(t.Context(), []string{"clone", "sandbox1"}); err != nil {
		t.Fatalf("clone of a paused sandbox: %v", err)
	}
	if got := d.repoSvc.(*fakeLifecycleRepo).sb; got.State != models.StatePaused || got.Snapshot == "" {
		t.Errorf("the source's record changed to %+v", got)
	}
}

func TestCloneRefusesASourceThatIsUp(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StateCreated} {
		sb := cloneSource()
		sb.State = state
		app, d := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, sb)

		err := app.Run(t.Context(), []string{"clone", "sandbox1"})
		if err == nil || !strings.Contains(err.Error(), "stop it first") {
			t.Errorf("clone of a %s sandbox returned %v, want a refusal that names the stop", state, err)
		}
		if slices.Contains(d.repoSvc.(*fakeLifecycleRepo).r.calls, "repo.Create") {
			t.Error("the refusal came after a record was created")
		}
	}
}

func TestCloneRefusesABadName(t *testing.T) {
	app, _ := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, cloneSource())

	if err := app.Run(t.Context(), []string{"clone", "--name", "", "sandbox1"}); err == nil {
		t.Error("clone accepted an empty name")
	}
}

// Everything claimed before the start goes back when it fails, and the source is left alone.
func TestCloneGivesBackWhatItClaimedWhenTheStartFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Clone"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, cloneSource())

	if err := app.Run(t.Context(), []string{"clone", "sandbox1"}); err == nil {
		t.Fatal("clone reported success when the start failed")
	}

	want := []string{"provider.Clone", "provider.Remove", "net.Release", "repo.Delete"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the teardown ran as %v, want %v", got, want)
	}

	repo := d.repoSvc.(*fakeLifecycleRepo)
	if repo.deletedID != "sandbox2" {
		t.Errorf("the teardown deleted %q, want the clone's record and never the source's", repo.deletedID)
	}
	if repo.sb.State != models.StateStopped {
		t.Errorf("the source's record changed to %s", repo.sb.State)
	}
}

// The clone is live once the start returns, so a failure after it keeps the sandbox and its record.
func TestCloneKeepsTheSandboxWhenTheRecordWriteFails(t *testing.T) {
	r := &recorder{fail: []string{"provider.Status"}}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, cloneSource())

	if err := app.Run(t.Context(), []string{"clone", "sandbox1"}); err == nil {
		t.Fatal("clone reported success when the status read failed")
	}

	if slices.Contains(r.calls, "provider.Remove") || slices.Contains(r.calls, "repo.Delete") {
		t.Errorf("a live clone was torn down: %v", r.calls)
	}
	// The record must name the netns and the interface, or a later rm cannot give them back.
	got := d.repoSvc.(*fakeLifecycleRepo).created
	if got.NetnsPath != "/run/netns/sandbox2" || got.Address.String() != "10.0.0.2/24" || got.HostInterface != "veth-sandbox2" {
		t.Errorf("the clone's record holds the network %+v, want the one it was allocated", got)
	}
}

func TestCloneCarriesThePolicyAndTellsTheHostBeforeTheStart(t *testing.T) {
	r := &recorder{}
	source := stopped()
	source.Policy = "locked"
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, source)

	if err := app.Run(t.Context(), []string{"clone", "sandbox1"}); err != nil {
		t.Fatalf("clone: %v", err)
	}

	want := []string{"net.Allocate", "net.Reapply", "provider.Clone"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the network was driven as %v, want %v", got, want)
	}
	if got := d.repoSvc.(*fakeLifecycleRepo).created.Policy; got != "locked" {
		t.Errorf("the clone names policy %q, want the source's", got)
	}
}
