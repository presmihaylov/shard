// Package conformance is the suite every models.Provider must pass: keep-alive, and Capabilities matching the verbs.
package conformance

import (
	"errors"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// Subject is what a provider's tests hand to Run.
type Subject struct {
	Provider models.Provider
	// NewSpec returns a fresh spec whose entrypoint exits 0 quickly. Its t.Cleanup must tolerate a sandbox a subtest already removed.
	NewSpec func(t *testing.T) models.SandboxSpec
	// SnapshotDir returns an empty directory the suite may write a snapshot into.
	SnapshotDir func(t *testing.T) string
}

// Run executes the suite. A verb with a false capability must refuse before its subtest skips.
func Run(t *testing.T, s Subject) {
	t.Helper()

	if s.Provider == nil || s.NewSpec == nil || s.SnapshotDir == nil {
		t.Fatal("conformance: Subject needs Provider, NewSpec and SnapshotDir")
	}

	caps := s.Provider.Capabilities()

	t.Run("CapabilitiesAreCoherent", func(t *testing.T) {
		// Both verbs need a snapshot, and only Pause makes one.
		if caps.Resume && !caps.Pause {
			t.Error("Resume: true with Pause: false; nothing can make the snapshot")
		}

		if caps.Fork && !caps.Pause {
			t.Error("Fork: true with Pause: false; nothing can make the snapshot")
		}
	})

	t.Run("Lifecycle", func(t *testing.T) {
		id := s.running(t)

		status, err := s.Provider.Wait(t.Context(), id)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}

		if status.Code != 0 {
			t.Errorf("Wait: got exit code %d, want 0", status.Code)
		}

		if !s.alive(t, id) {
			t.Fatal("the sandbox died with its entrypoint, and it must outlive it")
		}

		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if s.alive(t, id) {
			t.Fatal("the sandbox is still alive after Stop")
		}

		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	})

	t.Run("Pause", func(t *testing.T) {
		id := s.running(t)
		err := s.Provider.Pause(t.Context(), id, s.SnapshotDir(t))
		s.check(t, "Pause", caps.Pause, err)
	})

	t.Run("Resume", func(t *testing.T) {
		id := s.running(t)
		dir := s.snapshotOf(t, id, caps.Pause)
		err := s.Provider.Resume(t.Context(), id, dir)
		s.check(t, "Resume", caps.Resume, err)
	})

	t.Run("Fork", func(t *testing.T) {
		id := s.running(t)
		dir := s.snapshotOf(t, id, caps.Pause)
		_, err := s.Provider.Fork(t.Context(), dir, s.NewSpec(t))
		s.check(t, "Fork", caps.Fork, err)
	})
}

// Long enough that a provider which ignores grace still passes, short enough to keep the suite quick.
const stopGrace = 5 * time.Second

// Only Stop ends a sandbox, so this is the assertion the whole keep-alive default rests on.
func (s Subject) alive(t *testing.T, id string) bool {
	t.Helper()

	alive, err := s.Provider.Alive(t.Context(), id)
	if err != nil {
		t.Fatalf("Alive: %v", err)
	}

	return alive
}

// Create and Start are required verbs, so a failure here is a failure, never a skip.
func (s Subject) running(t *testing.T) string {
	t.Helper()

	spec := s.NewSpec(t)
	if _, err := s.Provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return spec.ID
}

// Returns an empty dir when the provider cannot pause, so Resume and Fork still have to refuse.
func (s Subject) snapshotOf(t *testing.T, id string, canPause bool) string {
	t.Helper()

	dir := s.SnapshotDir(t)
	if !canPause {
		return dir
	}

	if err := s.Provider.Pause(t.Context(), id, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	return dir
}

// The whole point of the suite: Capabilities and the verb must agree.
func (s Subject) check(t *testing.T, verb string, supported bool, err error) {
	t.Helper()

	name := s.Provider.Name()

	if !supported {
		if !errors.Is(err, models.ErrUnsupported) {
			t.Fatalf("%s reports %s unsupported, but %s returned %v, want ErrUnsupported", name, verb, verb, err)
		}

		t.Skipf("%s does not support %s on this host", name, verb)
	}

	if errors.Is(err, models.ErrUnsupported) {
		t.Fatalf("%s reports %s supported, but %s returned ErrUnsupported", name, verb, verb)
	}

	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
}
