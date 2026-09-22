// Package conformance is the suite every models.Provider must pass: keep-alive, the grace a stop
// owes an entrypoint, and Capabilities matching the verbs.
package conformance

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
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
	// Shell turns a shell script into the argv that runs it in the sandboxes NewSpec builds.
	Shell func(script string) []string
	// Scratch is a directory the sandbox's shell can write, for the files the suite leaves in one; empty is /.
	Scratch string
	// Reopen returns a second provider over the same substrate and state, which is what a daemon restart makes.
	Reopen func(t *testing.T) models.Provider
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
	// execCancelDelay is how long a command that outlives its caller runs before the cancellation lands.
	execCancelDelay = 500 * time.Millisecond
)

// forkCount is how many sandboxes one snapshot feeds at once: three, so nothing in a provider can count on a pair.
const forkCount = 3

// Run executes the suite. A verb with a false capability must refuse before its subtest skips.
func Run(t *testing.T, s Subject) {
	t.Helper()

	if s.Provider == nil || s.NewSpec == nil || s.NewIgnoresTermSpec == nil || s.SnapshotDir == nil || s.Shell == nil || s.Reopen == nil {
		t.Fatal("conformance: Subject needs Provider, NewSpec, NewIgnoresTermSpec, SnapshotDir, Shell and Reopen")
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

	t.Run("TheEntrypointIsStartedAgainUnderARestartPolicy", func(t *testing.T) {
		spec := s.NewSpec(t)
		spec.Entrypoint = s.Shell("exit 3")
		spec.Restart = models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 1}
		id := s.start(t, spec)

		count := s.awaitRestarts(t, id, func(count models.RestartCount) bool { return count.GaveUp })
		if count.Count != 2 || count.LastAt.IsZero() {
			t.Errorf("Restarts reads %+v at the give-up, want 2 starts again with a time on the last", count)
		}
		if !s.status(t, id).Alive() {
			t.Error("the sandbox ended at the give-up, which only a stop may do")
		}

		exit, err := s.Provider.Wait(t.Context(), id)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if exit.Code != 3 {
			t.Errorf("Wait reads code %d after the give-up, want 3 from the last run", exit.Code)
		}
		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		// The next run under this state directory must not inherit what the last one counted.
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("the second Create: %v", err)
		}
		if count := s.restarts(t, id); count != (models.RestartCount{}) {
			t.Errorf("Restarts reads %+v after a second Create, want zero", count)
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

	// The exit code is the whole reason exec is useful, and it is the opposite of what a create reports.
	t.Run("ExecReturnsTheCommandExitCode", func(t *testing.T) {
		id := s.running(t)

		status, out := s.exec(t, id, models.ExecSpec{Argv: s.Shell("echo one; exit 7")})

		if status.Code != 7 {
			t.Errorf("Exec: got exit code %d, want 7", status.Code)
		}
		if !strings.Contains(out, "one") {
			t.Errorf("Exec wrote %q, and the command printed \"one\"", out)
		}
	})

	// An exec is a second process in the same sandbox, so what one writes the next one reads.
	t.Run("TwoExecsShareTheSandbox", func(t *testing.T) {
		id := s.running(t)

		if status, _ := s.exec(t, id, models.ExecSpec{Argv: s.Shell("echo shared > " + s.scratch("conformance-exec"))}); status.Code != 0 {
			t.Fatalf("the first Exec exited %d", status.Code)
		}

		status, out := s.exec(t, id, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-exec"))})
		if status.Code != 0 {
			t.Fatalf("the second Exec exited %d, so it did not read what the first wrote", status.Code)
		}
		if !strings.Contains(out, "shared") {
			t.Errorf("the second Exec read %q, want what the first wrote", out)
		}
	})

	// An exec's env and workdir belong to that process alone, and the entrypoint never sees them.
	t.Run("ExecAppliesItsOwnEnvAndWorkDir", func(t *testing.T) {
		id := s.running(t)

		spec := models.ExecSpec{Argv: s.Shell("pwd; echo $CONFORMANCE"), Env: []string{"CONFORMANCE=set"}, WorkDir: "/tmp"}
		status, out := s.exec(t, id, spec)

		if status.Code != 0 {
			t.Fatalf("Exec exited %d", status.Code)
		}
		if !strings.Contains(out, "/tmp") {
			t.Errorf("Exec ran in %q, want /tmp", out)
		}
		if !strings.Contains(out, "set") {
			t.Errorf("Exec printed %q, and CONFORMANCE was set to \"set\"", out)
		}
	})

	// Refuse, never downgrade: an exec into nothing is an error, not an exit code.
	t.Run("ExecRefusesAnIdTheSubstrateNeverHeld", func(t *testing.T) {
		_, err := s.Provider.Exec(t.Context(), "conformance-never-created", models.ExecSpec{Argv: s.Shell("true")})
		if err == nil {
			t.Fatal("Exec accepted an id the substrate never held")
		}
		if !strings.Contains(err.Error(), "conformance-never-created") {
			t.Errorf("the refusal is %q, and it must name the sandbox", err)
		}
	})

	// A cancelled exec ends the command it started and nothing else: only Stop ends a sandbox.
	t.Run("ACancelledExecLeavesTheSandboxRunning", func(t *testing.T) {
		id := s.running(t)

		ctx, cancel := context.WithTimeout(t.Context(), execCancelDelay)
		defer cancel()

		if status, err := s.Provider.Exec(ctx, id, models.ExecSpec{Argv: s.Shell("sleep 60")}); err == nil {
			t.Fatalf("a cancelled Exec reported %+v, want the cancellation", status)
		}

		status, err := s.Provider.Status(t.Context(), id)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !status.Alive() {
			t.Errorf("the sandbox is %+v after a cancelled exec, and only Stop ends one", status)
		}
	})

	t.Run("ExecRefusesASandboxThatIsStopped", func(t *testing.T) {
		id := s.running(t)
		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		_, err := s.Provider.Exec(t.Context(), id, models.ExecSpec{Argv: s.Shell("true")})
		if err == nil {
			t.Fatal("Exec ran a command in a sandbox that is stopped")
		}
		if !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), string(models.StateStopped)) {
			t.Errorf("the refusal is %q, and it must name the sandbox and its state", err)
		}
	})

	t.Run("CloneRunsTheEntrypointAgainOverWhatTheSourceKept", func(t *testing.T) {
		source := s.running(t)
		if status, _ := s.exec(t, source, models.ExecSpec{Argv: s.Shell("echo kept > " + s.scratch("conformance-clone"))}); status.Code != 0 {
			t.Fatalf("the write into the source exited %d", status.Code)
		}
		if err := s.Provider.Stop(t.Context(), source, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		clone := copyOf(s.NewSpec(t))
		if err := s.Provider.Clone(t.Context(), source, clone); err != nil {
			t.Fatalf("Clone: %v", err)
		}

		// A Create under the clone's id would pass everything below but this: the file is the source's.
		if _, out := s.exec(t, clone.ID, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-clone"))}); !strings.Contains(out, "kept") {
			t.Errorf("the clone reads %q from the file the source wrote, want kept", out)
		}

		// The entrypoint exits 0 on its own, and only a fresh run of it can say so under the new id.
		exit, err := s.Provider.Wait(t.Context(), clone.ID)
		if err != nil {
			t.Fatalf("Wait on the clone: %v", err)
		}
		if exit.Code != 0 {
			t.Errorf("the clone's entrypoint exited %d, want 0", exit.Code)
		}
		if err := s.Provider.Stop(t.Context(), clone.ID, stopGrace); err != nil {
			t.Fatalf("Stop the clone: %v", err)
		}

		status, err := s.Provider.Status(t.Context(), source)
		if err != nil {
			t.Fatalf("Status of the source: %v", err)
		}
		if status.Alive() {
			t.Error("the clone brought the source back up")
		}
	})

	t.Run("CloneRefusesASourceThatIsRunning", func(t *testing.T) {
		source := s.running(t)
		err := s.Provider.Clone(t.Context(), source, s.NewSpec(t))
		if err == nil {
			t.Fatal("Clone copied a running sandbox")
		}
		if !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), string(models.StateRunning)) {
			t.Errorf("the refusal is %q, and it must name the sandbox and its state", err)
		}
	})

	// A daemon restart opens a new provider over what the last one left, and a running sandbox goes on as it was.
	t.Run("ANewProviderAdoptsARunningSandbox", func(t *testing.T) {
		spec := s.NewSpec(t)
		spec.Entrypoint = s.Shell("while true; do echo tick; sleep 0.2; done")
		id := s.start(t, spec)
		logged := s.awaitLog(t, id, 0)

		again := s.Reopen(t)
		status, err := again.Status(t.Context(), id)
		if err != nil {
			t.Fatalf("Status over the new provider: %v", err)
		}
		if !status.Alive() || status.PID <= 0 {
			t.Fatalf("the new provider sees %+v, want the sandbox running with its pid", status)
		}

		// The entrypoint's output must keep landing in the log, on the connection the new provider opened.
		s.awaitLog(t, id, logged)

		out, err := os.CreateTemp(t.TempDir(), "exec-output")
		if err != nil {
			t.Fatalf("create a file for the exec output: %v", err)
		}
		defer out.Close()
		exit, err := again.Exec(t.Context(), id, models.ExecSpec{Argv: s.Shell("echo adopted"), Stdout: out, Stderr: out})
		if err != nil {
			t.Fatalf("Exec over the new provider: %v", err)
		}
		written, err := os.ReadFile(out.Name())
		if err != nil || exit.Code != 0 || !strings.Contains(string(written), "adopted") {
			t.Fatalf("Exec over the new provider = %+v, %q, %v", exit, written, err)
		}

		if err := again.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop over the new provider: %v", err)
		}
		if s.status(t, id).Alive() {
			t.Fatal("the first provider still sees the sandbox alive after the new one stopped it")
		}
	})

	t.Run("Pause", func(t *testing.T) {
		id := s.running(t)
		err := s.Provider.Pause(t.Context(), id, s.SnapshotDir(t))
		s.check(t, models.VerbPause, caps.Pause, err)
	})

	// A snapshot is of a running sandbox on every substrate, so nothing else is a source for one.
	t.Run("PauseRefusesASandboxThatNeverStarted", func(t *testing.T) {
		if !caps.Pause {
			t.Skipf("%s does not support %s on this host", s.Provider.Name(), models.VerbPause)
		}

		spec := s.NewSpec(t)
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Provider.Pause(t.Context(), spec.ID, s.SnapshotDir(t)); err == nil {
			t.Error("Pause of a sandbox that never started = nil, want a refusal")
		}
		if state := s.status(t, spec.ID).State; state != models.StateCreated {
			t.Errorf("the refused sandbox is %q, and a refusal leaves it %q", state, models.StateCreated)
		}
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
		err := s.Provider.Fork(t.Context(), dir, copyOf(s.NewSpec(t)))
		s.check(t, models.VerbFork, caps.Fork, err)
	})

	// One snapshot feeds as many sandboxes as are asked of it, none of them is the source, and the source comes back after them.
	t.Run("ManyForksFromOneSnapshot", func(t *testing.T) {
		if !caps.Fork {
			t.Skipf("%s does not support %s on this host", s.Provider.Name(), models.VerbFork)
		}

		source := s.running(t)
		if status, _ := s.exec(t, source, models.ExecSpec{Argv: s.Shell("echo source > " + s.scratch("conformance-fork"))}); status.Code != 0 {
			t.Fatalf("the write into the source exited %d", status.Code)
		}

		dir := s.SnapshotDir(t)
		if err := s.Provider.Pause(t.Context(), source, dir); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if s.status(t, source).Alive() {
			t.Fatal("the source is still alive after a Pause, and its snapshot is what holds it now")
		}

		forks := make([]models.SandboxSpec, forkCount)
		for i := range forks {
			forks[i] = copyOf(s.NewSpec(t))
			if err := s.Provider.Fork(t.Context(), dir, forks[i]); err != nil {
				t.Fatalf("fork %d of %d from one snapshot: %v", i+1, forkCount, err)
			}
		}

		// Each fork is a sandbox of its own: its own process, and a command of its own that runs in it.
		pids := map[int]string{}
		for _, fork := range forks {
			status := s.status(t, fork.ID)
			if !status.Alive() || status.PID <= 0 {
				t.Fatalf("fork %s is %+v, want it running with a pid of its own", fork.ID, status)
			}
			if other, held := pids[status.PID]; held {
				t.Errorf("fork %s runs as pid %d, which fork %s already holds", fork.ID, status.PID, other)
			}
			pids[status.PID] = fork.ID

			if _, out := s.exec(t, fork.ID, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-fork"))}); !strings.Contains(out, "source") {
				t.Errorf("fork %s reads %q from the file the source wrote, want source", fork.ID, out)
			}
		}

		// Only Stop ends a sandbox, and it ends the one it names.
		if err := s.Provider.Stop(t.Context(), forks[0].ID, stopGrace); err != nil {
			t.Fatalf("Stop the first fork: %v", err)
		}
		for _, fork := range forks[1:] {
			if !s.status(t, fork.ID).Alive() {
				t.Errorf("fork %s went down with the fork that was stopped", fork.ID)
			}
			if err := s.Provider.Stop(t.Context(), fork.ID, stopGrace); err != nil {
				t.Fatalf("Stop fork %s: %v", fork.ID, err)
			}
		}

		// The forks consumed nothing: the same snapshot still brings the source back.
		if err := s.Provider.Resume(t.Context(), source, dir); err != nil {
			t.Fatalf("Resume the source after %d forks: %v", forkCount, err)
		}
		if !s.status(t, source).Alive() {
			t.Fatal("the source is not running after a Resume from the snapshot its forks came from")
		}
		if _, out := s.exec(t, source, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-fork"))}); !strings.Contains(out, "source") {
			t.Errorf("the source reads %q from the file it wrote before the pause, want source", out)
		}
		if err := s.Provider.Stop(t.Context(), source, stopGrace); err != nil {
			t.Fatalf("Stop the source: %v", err)
		}
	})
}

// copyOf is the spec the orchestrator hands Clone and Fork: the copy's id, name, lease and bounds, and no entrypoint, which the source keeps.
func copyOf(spec models.SandboxSpec) models.SandboxSpec {
	return models.SandboxSpec{ID: spec.ID, Name: spec.Name, StateDir: spec.StateDir, Network: spec.Network, Resources: spec.Resources}
}

func (s Subject) scratch(name string) string {
	if s.Scratch == "" {
		return "/" + name
	}

	return path.Join(s.Scratch, name)
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

// awaitLog blocks until the sandbox log holds more than seen bytes, and returns how many it holds.
func (s Subject) awaitLog(t *testing.T, id string, seen int) int {
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
		if len(out) > seen {
			return len(out)
		}

		time.Sleep(readyPoll)
	}

	t.Fatalf("the log of %s did not grow past %d bytes within %s", id, seen, waitSlack)

	return seen
}

// exec runs one command in a sandbox and returns how it ended, with everything it wrote. The spec
// takes files rather than pipes, because a TTY hands the guest one pty replica.
func (s Subject) exec(t *testing.T, id string, spec models.ExecSpec) (models.ExitStatus, string) {
	t.Helper()

	out, err := os.CreateTemp(t.TempDir(), "exec-output")
	if err != nil {
		t.Fatalf("create a file for the exec output: %v", err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close the exec output file: %v", err)
		}
	}()

	spec.Stdout, spec.Stderr = out, out

	status, err := s.Provider.Exec(t.Context(), id, spec)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	written, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatalf("read the exec output: %v", err)
	}

	return status, string(written)
}

func (s Subject) restarts(t *testing.T, id string) models.RestartCount {
	t.Helper()

	count, err := s.Provider.Restarts(t.Context(), id)
	if err != nil {
		t.Fatalf("Restarts: %v", err)
	}

	return count
}

// awaitRestarts polls the count until it says what the subtest wants, or the slack runs out.
func (s Subject) awaitRestarts(t *testing.T, id string, want func(models.RestartCount) bool) models.RestartCount {
	t.Helper()

	deadline := time.Now().Add(waitSlack)
	for time.Now().Before(deadline) {
		count := s.restarts(t, id)
		if want(count) {
			return count
		}

		time.Sleep(readyPoll)
	}

	t.Fatalf("the restart count of %s never read as wanted within %s", id, waitSlack)

	return models.RestartCount{}
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
