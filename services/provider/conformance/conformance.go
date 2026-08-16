// Package conformance is the shared test suite every models.Provider must pass.
//
// It exists for one reason: a provider can report Fork: true from Capabilities
// and return ErrUnsupported from Fork, and no compiler can catch that. This
// suite calls each verb and checks that the two answers agree.
//
// A provider's own test package supplies a Subject and calls Run from a test
// behind //go:build integration, because every real verb here needs a box.
package conformance

import (
	"errors"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// Subject is what a provider's tests hand to Run. The suite drives the verbs
// itself; the provider only supplies the host-shaped things it cannot invent.
type Subject struct {
	// Provider under test, built by its own constructor so Capabilities has
	// already probed this host.
	Provider models.Provider

	// NewSpec returns a spec for one new sandbox: a fresh ID, network, state
	// directory and rootfs. The suite calls it for every sandbox it needs,
	// including the one Fork creates, so it must not return the same ID twice.
	// Register teardown with t.Cleanup.
	NewSpec func(t *testing.T) models.SandboxSpec

	// SnapshotDir returns an empty directory the suite may write a snapshot into.
	SnapshotDir func(t *testing.T) string
}

// Run executes the suite. Optional verbs whose capability is false are skipped
// after the suite has checked that they refuse; required verbs are always run.
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

		if err := s.Provider.Kill(t.Context(), id, killSignal); err != nil {
			t.Fatalf("Kill: %v", err)
		}

		if _, err := s.Provider.Wait(t.Context(), id); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		if err := s.Provider.Delete(t.Context(), id); err != nil {
			t.Fatalf("Delete: %v", err)
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

// killSignal is SIGKILL. The suite tests that Kill reaches the sandbox, not that
// the entrypoint handles signals well.
const killSignal = 9

// running creates and starts one sandbox and returns its ID. Create and Start
// are required verbs, so a failure here is a failure of the provider, not a skip.
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

// snapshotOf pauses the sandbox and returns the snapshot directory. When the
// provider cannot pause, it returns an empty directory instead, so that Resume
// and Fork are still called and still have to refuse.
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

// check is the whole point of the suite: Capabilities and the verb must agree.
// It skips the rest of a subtest for a verb the provider does not have, but only
// after that verb has refused properly.
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
