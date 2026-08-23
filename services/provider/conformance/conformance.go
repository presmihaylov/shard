// Package conformance is the suite every models.Provider must pass: keep-alive, the grace a stop
// owes an entrypoint, and Capabilities matching the verbs.
package conformance

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// Subject is what a provider's tests hand to Run.
type Subject struct {
	Provider models.Provider
	// NewSpec returns a fresh spec whose entrypoint exits 0 quickly. Its t.Cleanup must tolerate a sandbox a subtest already removed.
	NewSpec func(t *testing.T) models.SandboxSpec
	// NewIgnoresTermSpec returns a spec whose entrypoint ignores SIGTERM, which is what proves grace.
	// It must print ReadyMarker on stdout once the entrypoint refuses the signal, and not before.
	NewIgnoresTermSpec func(t *testing.T) models.SandboxSpec
	// SnapshotDir returns an empty directory the suite may write a snapshot into.
	SnapshotDir func(t *testing.T) string
}

// ReadyMarker is what an ignores-term entrypoint prints once it refuses SIGTERM. A stop sent before
// that lands on an entrypoint that still dies on the signal, which proves nothing about grace.
const ReadyMarker = "conformance-ignores-term"

const (
	// stopGrace is what a cooperative entrypoint never needs. A provider that ignores grace waits it out.
	stopGrace = 5 * time.Second
	// termGrace is what an entrypoint that refuses SIGTERM is owed, and slack is the kill after it.
	termGrace = 3 * time.Second
	killSlack = 15 * time.Second
	// waitSlack bounds the assertions that must answer at once rather than poll.
	waitSlack = 30 * time.Second
	// readyPoll paces the wait for the marker, which arrives as fast as the guest shell starts.
	readyPoll = 20 * time.Millisecond
)

// Run executes the suite. A verb with a false capability must refuse before its subtest skips.
func Run(t *testing.T, s Subject) {
	t.Helper()

	if s.Provider == nil || s.NewSpec == nil || s.NewIgnoresTermSpec == nil || s.SnapshotDir == nil {
		t.Fatal("conformance: Subject needs Provider, NewSpec, NewIgnoresTermSpec and SnapshotDir")
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

		// A snapshot nothing can restore is not a capability.
		if caps.Pause && !caps.Resume {
			t.Error("Pause: true with Resume: false; nothing can restore the snapshot")
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

		if !s.status(t, id).Alive() {
			t.Fatal("the sandbox died with its entrypoint, and it must outlive it")
		}

		started := time.Now()
		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		// The entrypoint is already gone, so a stop that costs its grace ended the sandbox with a kill.
		if elapsed := time.Since(started); elapsed > stopGrace/3 {
			t.Errorf("Stop took %s of a %s grace, so the signal did not end the sandbox", elapsed, stopGrace)
		}

		if s.status(t, id).Alive() {
			t.Fatal("the sandbox is still alive after Stop")
		}

		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	})

	t.Run("StatusAfterCreate", func(t *testing.T) {
		spec := s.NewSpec(t)
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}

		status := s.status(t, spec.ID)
		if !status.Exists {
			t.Fatal("Status: a sandbox that was just created does not exist on the substrate")
		}
		if status.State != models.StateCreated {
			t.Errorf("Status: got state %q, want %q", status.State, models.StateCreated)
		}
		if status.PID <= 0 {
			t.Errorf("Status: got pid %d, and the sandbox process has a real one", status.PID)
		}
	})

	t.Run("StatusOfAnIdTheSubstrateNeverHeld", func(t *testing.T) {
		status, err := s.Provider.Status(t.Context(), "conformance-never-created")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}

		if status.Exists || status.Alive() {
			t.Errorf("Status reported %+v for an id the substrate never held", status)
		}
	})

	// runsc refuses to signal a container whose entrypoint never started, and every substrate has a
	// state like it. Stop is the only thing that ends a sandbox, so it must end that one too.
	t.Run("StopASandboxThatNeverStarted", func(t *testing.T) {
		spec := s.NewIgnoresTermSpec(t)
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}

		if err := s.Provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if s.status(t, spec.ID).Alive() {
			t.Error("the sandbox is still alive after a Stop that never started it")
		}
	})

	// Stop is what a caller retries, so it must answer the same way every time.
	t.Run("StopIsIdempotentAndSurvivesARemove", func(t *testing.T) {
		id := s.running(t)

		for _, when := range []string{"first", "second"} {
			if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
				t.Fatalf("the %s Stop: %v", when, err)
			}
		}

		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Errorf("Stop after Remove: %v", err)
		}
	})

	// Remove force-ends a running sandbox. shard rm needs it, and nothing else drops the rootfs.
	t.Run("RemoveARunningSandbox", func(t *testing.T) {
		id := s.running(t)

		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		if s.status(t, id).Alive() {
			t.Error("the sandbox is still alive after Remove")
		}
	})

	// The grace is the second argument of a required verb, so a provider that ignores it must fail here.
	t.Run("StopOwnsTheGraceAndThenKills", func(t *testing.T) {
		spec := s.NewIgnoresTermSpec(t)
		id := s.start(t, spec)
		s.awaitReady(t, id)

		started := time.Now()
		if err := s.Provider.Stop(t.Context(), id, termGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		elapsed := time.Since(started)

		if elapsed < termGrace {
			t.Errorf("Stop took %s, and an entrypoint that ignores SIGTERM is owed its whole %s grace", elapsed, termGrace)
		}
		if elapsed > termGrace+killSlack {
			t.Errorf("Stop took %s of a %s grace, so nothing killed the entrypoint that ignored the signal", elapsed, termGrace)
		}

		if s.status(t, id).Alive() {
			t.Error("the sandbox is still alive after Stop")
		}
	})

	// A second Create over a used state directory must not let the first run's exit answer a wait.
	t.Run("ASecondCreateAnswersNoStaleExitStatus", func(t *testing.T) {
		spec := s.NewSpec(t)
		id := s.start(t, spec)

		if _, err := s.Provider.Wait(t.Context(), id); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("the second Create: %v", err)
		}

		// Nothing has started, so the only status a wait could find is the one the first run left.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		if status, err := s.Provider.Wait(ctx, id); err == nil {
			t.Errorf("Wait answered %+v before the second run started its entrypoint", status)
		}
	})

	t.Run("WaitReturnsACancelledContext", func(t *testing.T) {
		id := s.start(t, s.NewIgnoresTermSpec(t))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		done := make(chan error, 1)
		go func() {
			_, err := s.Provider.Wait(ctx, id)
			done <- err
		}()

		select {
		case err := <-done:
			if !errors.Is(err, ctx.Err()) {
				t.Errorf("Wait returned %v, want the context error", err)
			}
		case <-time.After(waitSlack):
			t.Fatal("Wait never answered on an already cancelled context")
		}
	})

	t.Run("Pause", func(t *testing.T) {
		id := s.running(t)
		err := s.Provider.Pause(t.Context(), id, s.SnapshotDir(t))
		s.check(t, models.VerbPause, caps.Pause, err)
	})

	t.Run("Resume", func(t *testing.T) {
		id := s.running(t)
		dir := s.snapshotOf(t, id, caps.Pause)
		err := s.Provider.Resume(t.Context(), id, dir)
		s.check(t, models.VerbResume, caps.Resume, err)
	})

	t.Run("Fork", func(t *testing.T) {
		id := s.running(t)
		dir := s.snapshotOf(t, id, caps.Pause)
		err := s.Provider.Fork(t.Context(), dir, s.NewSpec(t))
		s.check(t, models.VerbFork, caps.Fork, err)
	})
}

// awaitReady blocks until the entrypoint has printed ReadyMarker, which is the only proof the suite
// can read that it now ignores SIGTERM.
func (s Subject) awaitReady(t *testing.T, id string) {
	t.Helper()

	path, err := s.Provider.LogPath(id)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}

	deadline := time.Now().Add(waitSlack)
	for time.Now().Before(deadline) {
		out, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read the sandbox log %s: %v", path, err)
		}
		if strings.Contains(string(out), ReadyMarker) {
			return
		}

		time.Sleep(readyPoll)
	}

	t.Fatalf("the entrypoint of %s never printed %q within %s", id, ReadyMarker, waitSlack)
}

// Only Stop ends a sandbox, so Status is the assertion the whole keep-alive default rests on.
func (s Subject) status(t *testing.T, id string) models.Status {
	t.Helper()

	status, err := s.Provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	return status
}

// Create and Start are required verbs, so a failure here is a failure, never a skip.
func (s Subject) running(t *testing.T) string {
	t.Helper()

	return s.start(t, s.NewSpec(t))
}

func (s Subject) start(t *testing.T, spec models.SandboxSpec) string {
	t.Helper()

	if err := s.Provider.Create(t.Context(), spec); err != nil {
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

// The whole point of the suite: Capabilities and the verb must agree, and a refusal must name both.
func (s Subject) check(t *testing.T, verb string, supported bool, err error) {
	t.Helper()

	name := s.Provider.Name()

	if !supported {
		var refusal *models.UnsupportedError
		if !errors.As(err, &refusal) {
			t.Fatalf("%s reports %s unsupported, but %s returned %v, want an UnsupportedError", name, verb, verb, err)
		}
		if refusal.Provider != name {
			t.Errorf("the refusal names provider %q, want %q", refusal.Provider, name)
		}
		if refusal.Verb != verb {
			t.Errorf("the refusal names verb %q, want %q", refusal.Verb, verb)
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
