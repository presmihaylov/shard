// Package conformance is the suite every models.Provider must pass: keep-alive, named processes, the grace a stop owes them, and Capabilities matching the verbs.
package conformance

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// Subject is what a provider's tests hand to Run.
type Subject struct {
	Provider models.Provider
	// NewSpec returns a fresh spec. Its t.Cleanup must tolerate a sandbox a subtest already removed.
	NewSpec func(t *testing.T) models.SandboxSpec
	// EmptyDir returns an empty directory the suite may write a checkpoint or a snapshot into.
	EmptyDir func(t *testing.T) string
	// Shell turns a shell script into the argv that runs it in the sandboxes NewSpec builds.
	Shell func(script string) []string
	// Scratch is a directory the sandbox's shell can write, for the files the suite leaves in one; empty is /, and a set one is a fake guest's host directory every sandbox shares.
	Scratch string
	// SharedScratch says every sandbox's shell writes the one host Scratch, as a fake VM guest's does, so a copy's write cannot be told from its source's.
	SharedScratch bool
	// HostLayer says the guest's writable layer is a host directory the daemon writes; a fake VM guest execs on the host, so it stays false.
	HostLayer bool
	// Reopen returns a second provider over the same substrate and state, which is what a daemon restart makes; it may close Provider.
	Reopen func(t *testing.T) models.Provider
	// ReseedWindow is the guest uptime the fork source passes before its pause, so no early-boot crng reseed can split the forks; zero pauses at once.
	ReseedWindow time.Duration
	// RootHoldsEveryCapability says the runtime gives root every capability in its user namespace whatever the spec asks, as sysbox-runc does.
	RootHoldsEveryCapability bool
}

// environments is where the daemon rewrites a stopped sandbox's guest environment.
type environments interface {
	Environment(id string) (models.Environment, error)
}

// readyMarker is what an ignores-term process prints once it refuses SIGTERM; a stop before that proves nothing about grace.
const readyMarker = "conformance-ignores-term"

const (
	// stopGrace is what a cooperative process never needs. A provider that ignores grace waits it out.
	stopGrace = 5 * time.Second
	// termGrace is what a process that refuses SIGTERM is owed, and slack is the kill after it.
	termGrace = 3 * time.Second
	killSlack = 15 * time.Second
	// waitSlack bounds the assertions that must answer at once rather than poll.
	waitSlack = 30 * time.Second
	// readyPoll paces the wait for the marker, which arrives as fast as the guest shell starts.
	readyPoll = 20 * time.Millisecond
	// execCancelDelay is how long a command that outlives its caller runs before the cancellation lands.
	execCancelDelay = 500 * time.Millisecond
)

// forkCount is how many sandboxes one running source feeds: three, so nothing in a provider can count on a pair.
const forkCount = 3

// Run executes the suite. A verb with a false capability must refuse before its subtest skips.
func Run(t *testing.T, s Subject) {
	t.Helper()

	if s.Provider == nil || s.NewSpec == nil || s.EmptyDir == nil || s.Shell == nil || s.Reopen == nil {
		t.Fatal("conformance: Subject needs Provider, NewSpec, EmptyDir, Shell and Reopen")
	}

	caps := s.Provider.Capabilities()

	t.Run("CapabilitiesAreCoherent", func(t *testing.T) {
		// Resume needs a checkpoint, and only Pause makes one; a fork captures its running source itself (SHARD-457).
		if caps.Resume && !caps.Pause {
			t.Error("Resume: true with Pause: false; nothing can make the checkpoint")
		}

		// A checkpoint nothing can restore is not a capability.
		if caps.Pause && !caps.Resume {
			t.Error("Pause: true with Resume: false; nothing can restore the checkpoint")
		}
	})

	t.Run("Lifecycle", func(t *testing.T) {
		id := s.running(t)
		if !s.status(t, id).Alive() {
			t.Fatal("a started sandbox runs no process yet, and it must still be running")
		}

		s.run(t, id, models.ProcessSpec{Name: "job", Argv: s.Shell("exit 0")})
		job := s.awaitProcess(t, id, "job", hasEnded)
		if job.State != models.ProcessExited || job.Exit == nil || job.Exit.Code != 0 {
			t.Errorf("the process ended as %+v, want exited with code 0", job.ProcessStatus)
		}
		if !s.status(t, id).Alive() {
			t.Fatal("the sandbox died with its process, and it must outlive it")
		}

		started := time.Now()
		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		// Nothing runs, so a stop that costs its grace ended the sandbox with a kill.
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

	// Every substrate has a created state that runs nothing yet, and Stop is the only thing that ends a sandbox, so it must end that one too.
	t.Run("StopASandboxThatNeverStarted", func(t *testing.T) {
		spec := s.NewSpec(t)
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

	// Remove force-ends a running sandbox. shard remove needs it, and nothing else drops the rootfs.
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
		id := s.running(t)
		s.run(t, id, s.ignoresTerm("stubborn"))
		s.awaitPrinted(t, id, "stubborn", readyMarker)

		started := time.Now()
		if err := s.Provider.Stop(t.Context(), id, termGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		elapsed := time.Since(started)

		if elapsed < termGrace {
			t.Errorf("Stop took %s, and a process that ignores SIGTERM is owed its whole %s grace", elapsed, termGrace)
		}
		if elapsed > termGrace+killSlack {
			t.Errorf("Stop took %s of a %s grace, so nothing killed the process that ignored the signal", elapsed, termGrace)
		}

		if s.status(t, id).Alive() {
			t.Error("the sandbox is still alive after Stop")
		}
	})

	// The grace is a bound and never a wait: processes that exit on SIGTERM end the stop with them (SHARD-460).
	t.Run("StopEndsWhenEveryProcessExitsOnTerm", func(t *testing.T) {
		id := s.running(t)
		for _, name := range []string{"first", "second"} {
			s.run(t, id, models.ProcessSpec{Name: name, Argv: s.Shell("trap 'exit 0' TERM; echo conformance-exits-on-term; while true; do sleep 0.1; done")})
			s.awaitPrinted(t, id, name, "conformance-exits-on-term")
		}

		started := time.Now()
		if err := s.Provider.Stop(t.Context(), id, models.StopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if elapsed := time.Since(started); elapsed > stopGrace/3 {
			t.Errorf("Stop took %s of the %s grace, so it waited past processes that exited on SIGTERM", elapsed, models.StopGrace)
		}
		if s.status(t, id).Alive() {
			t.Error("the sandbox is still alive after Stop")
		}
	})

	// A second Create over a used state directory must not show what the first run ran.
	t.Run("ASecondCreateHoldsNoProcessOfTheFirst", func(t *testing.T) {
		spec := s.NewSpec(t)
		id := s.start(t, spec)
		s.run(t, id, models.ProcessSpec{Name: "job", Argv: s.Shell("exit 0")})
		s.awaitProcess(t, id, "job", hasEnded)

		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := s.Provider.Remove(t.Context(), id); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("the second Create: %v", err)
		}
		if table := s.processes(t, s.Provider, id); len(table) != 0 {
			t.Errorf("Processes reads %+v after a second Create, want none", table)
		}
	})

	t.Run("AProcessIsStartedAgainUnderARestartPolicy", func(t *testing.T) {
		id := s.running(t)
		s.run(t, id, models.ProcessSpec{Name: "crash", Argv: s.Shell("exit 3"), Restart: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 1}})

		gaveUp := s.awaitProcess(t, id, "crash", hasEnded)
		if gaveUp.State != models.ProcessGaveUp || gaveUp.Restarts != 2 {
			t.Errorf("the process ended as %+v, want it given up after 2 starts again", gaveUp.ProcessStatus)
		}
		if gaveUp.Exit == nil || gaveUp.Exit.Code != 3 {
			t.Errorf("the process gave up with %+v, want code 3 from the last run", gaveUp.Exit)
		}
		if !s.status(t, id).Alive() {
			t.Error("the sandbox ended at the give-up, which only a stop may do")
		}

		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		// The daemon reads the table after the stop, so the stop must leave it readable (SHARD-401).
		if after, held := s.process(t, s.Provider, id, "crash"); !held || after.State != gaveUp.State || after.Restarts != gaveUp.Restarts {
			t.Errorf("Processes reads %+v after Stop, want %+v as at the give-up", after, gaveUp)
		}
	})

	t.Run("TwoProcessesRunSideBySideWithALogEach", func(t *testing.T) {
		id := s.running(t)
		for _, name := range []string{"left", "right"} {
			s.run(t, id, models.ProcessSpec{Name: name, Argv: s.Shell("echo from-" + name + "; while true; do sleep 0.1; done")})
		}

		for name, other := range map[string]string{"left": "right", "right": "left"} {
			out := s.awaitPrinted(t, id, name, "from-"+name)
			if strings.Contains(out, "from-"+other) {
				t.Errorf("the log of %s holds what %s printed:\n%s", name, other, out)
			}
			// The log can reach the host before the table does, as on Firecracker, whose host writes the table off the guest's reports.
			s.awaitProcess(t, id, name, isRunning)
		}
	})

	// A kill ends the one process it names, owes it the grace, and never starts it again.
	t.Run("StopProcessOwnsTheGraceAndLeavesTheOthersRunning", func(t *testing.T) {
		id := s.running(t)
		always := models.RestartSpec{Policy: models.RestartAlways}
		s.run(t, id, models.ProcessSpec{Name: "steady", Argv: s.Shell("while true; do sleep 0.1; done"), Restart: always})
		stubborn := s.ignoresTerm("stubborn")
		stubborn.Restart = always
		s.run(t, id, stubborn)
		s.awaitPrinted(t, id, "stubborn", readyMarker)

		started := time.Now()
		if err := s.Provider.StopProcess(t.Context(), id, "stubborn", termGrace); err != nil {
			t.Fatalf("StopProcess: %v", err)
		}
		elapsed := time.Since(started)
		if elapsed < termGrace {
			t.Errorf("StopProcess took %s, and a process that ignores SIGTERM is owed its whole %s grace", elapsed, termGrace)
		}
		if elapsed > termGrace+killSlack {
			t.Errorf("StopProcess took %s of a %s grace, so nothing killed the process that ignored the signal", elapsed, termGrace)
		}

		killed := s.awaitProcess(t, id, "stubborn", hasEnded)
		if killed.State != models.ProcessKilled {
			t.Errorf("the stopped process reads %+v, want killed and never started again", killed.ProcessStatus)
		}
		if steady, _ := s.process(t, s.Provider, id, "steady"); steady.State != models.ProcessRunning {
			t.Errorf("the other process reads %+v, want it running on", steady.ProcessStatus)
		}
		if !s.status(t, id).Alive() {
			t.Error("the sandbox ended with one of its processes, which only a stop may do")
		}
	})

	t.Run("StopProcessWithNoGraceKillsAtOnce", func(t *testing.T) {
		id := s.running(t)
		s.run(t, id, s.ignoresTerm("stubborn"))
		s.awaitPrinted(t, id, "stubborn", readyMarker)

		started := time.Now()
		if err := s.Provider.StopProcess(t.Context(), id, "stubborn", 0); err != nil {
			t.Fatalf("StopProcess: %v", err)
		}
		if elapsed := time.Since(started); elapsed > stopGrace/3 {
			t.Errorf("StopProcess with no grace took %s, so it waited for a SIGTERM the process ignores", elapsed)
		}

		killed := s.awaitProcess(t, id, "stubborn", hasEnded)
		if killed.State != models.ProcessKilled || killed.Exit == nil || killed.Exit.Signal != int(syscall.SIGKILL) {
			t.Errorf("the process reads %+v, want killed by SIGKILL", killed.ProcessStatus)
		}
	})

	// A name is one process at a time, and a run of an ended name starts it over.
	t.Run("StartProcessRefusesANameThatStillRuns", func(t *testing.T) {
		id := s.running(t)
		spec := models.ProcessSpec{Name: "only", Argv: s.Shell("while true; do sleep 0.1; done")}
		s.run(t, id, spec)

		if err := s.Provider.StartProcess(t.Context(), id, spec); !errors.Is(err, models.ErrProcessRunning) {
			t.Errorf("a second StartProcess of a name that runs = %v, want ErrProcessRunning", err)
		}

		if err := s.Provider.StopProcess(t.Context(), id, "only", 0); err != nil {
			t.Fatalf("StopProcess: %v", err)
		}
		s.awaitProcess(t, id, "only", hasEnded)
		s.run(t, id, spec)
		if again := s.awaitProcess(t, id, "only", isRunning); again.Restarts != 0 {
			t.Errorf("the run of an ended name reads %+v, want a fresh process", again.ProcessStatus)
		}
	})

	// The refusal is what shard run answers with 127, the code a shell gives a command it cannot find.
	t.Run("StartProcessRefusesACommandThatIsNotThere", func(t *testing.T) {
		id := s.running(t)

		err := s.Provider.StartProcess(t.Context(), id, models.ProcessSpec{Name: "missing", Argv: []string{"/no/such/binary"}})
		refused, ok := errors.AsType[*models.CommandNotStartedError](err)
		if !ok {
			t.Fatalf("StartProcess returned %v, want a CommandNotStartedError", err)
		}
		if refused.Code != models.CommandNotFoundExitCode {
			t.Errorf("the refusal carries code %d, want %d", refused.Code, models.CommandNotFoundExitCode)
		}
	})

	// A process's env and workdir belong to it alone, as an exec's do.
	t.Run("AProcessAppliesItsOwnEnvAndWorkDir", func(t *testing.T) {
		id := s.running(t)

		s.run(t, id, models.ProcessSpec{Name: "env", Argv: s.Shell("pwd; echo conformance-$CONFORMANCE"), Env: []string{"CONFORMANCE=set"}, WorkDir: "/tmp"})
		out := s.awaitPrinted(t, id, "env", "conformance-set")
		if !strings.Contains(out, "/tmp") {
			t.Errorf("the process ran in %q, want /tmp", out)
		}
	})

	t.Run("StartProcessRefusesASandboxThatIsStopped", func(t *testing.T) {
		id := s.running(t)
		if err := s.Provider.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		err := s.Provider.StartProcess(t.Context(), id, models.ProcessSpec{Name: "late", Argv: s.Shell("true")})
		if err == nil {
			t.Fatal("StartProcess ran a process in a sandbox that is stopped")
		}
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the refusal is %q, and it must name the sandbox", err)
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

	// An exec's env and workdir belong to that command alone.
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

	t.Run("SnapshotHoldsWhatTheSourceKeptAndOutlivesIt", func(t *testing.T) {
		source := s.NewSpec(t)
		s.start(t, source)
		if status, _ := s.exec(t, source.ID, models.ExecSpec{Argv: s.Shell("echo kept > " + s.scratch("conformance-snapshot"))}); status.Code != 0 {
			t.Fatalf("the write into the source exited %d", status.Code)
		}
		if err := s.Provider.Stop(t.Context(), source.ID, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		dir := s.EmptyDir(t)
		if err := s.Provider.Snapshot(t.Context(), source.ID, dir); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		// An rm takes the substrate's state and then the record's directory, and the snapshot must need neither.
		if err := s.Provider.Remove(t.Context(), source.ID); err != nil {
			t.Fatalf("Remove the source: %v", err)
		}
		if err := os.RemoveAll(source.StateDir); err != nil {
			t.Fatalf("remove the source's state directory: %v", err)
		}

		first, second := s.seeded(t, dir), s.seeded(t, dir)
		for _, id := range []string{first, second} {
			if _, out := s.exec(t, id, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-snapshot"))}); !strings.Contains(out, "kept") {
				t.Errorf("sandbox %s reads %q from the file the source wrote, want kept", id, out)
			}
		}

		// A scratch directory is a fake guest's host directory, which every sandbox shares.
		if s.Scratch != "" {
			return
		}
		if status, _ := s.exec(t, first, models.ExecSpec{Argv: s.Shell("echo mine > " + s.scratch("conformance-snapshot"))}); status.Code != 0 {
			t.Fatalf("the write into %s exited %d", first, status.Code)
		}
		if _, out := s.exec(t, second, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-snapshot"))}); !strings.Contains(out, "kept") {
			t.Errorf("sandbox %s reads %q after a write in %s, want its own kept", second, out, first)
		}
	})

	t.Run("SnapshotKeepsASymlinkToTheHostAsALink", func(t *testing.T) {
		host := path.Join(t.TempDir(), "conformance-host-secret")
		if err := os.WriteFile(host, []byte("host only\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		source := s.running(t)
		link := s.scratch("conformance-escape")
		if status, _ := s.exec(t, source, models.ExecSpec{Argv: s.Shell("ln -sf " + host + " " + link)}); status.Code != 0 {
			t.Fatalf("the symlink in the source exited %d", status.Code)
		}
		if err := s.Provider.Stop(t.Context(), source, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		dir := s.EmptyDir(t)
		if err := s.Provider.Snapshot(t.Context(), source, dir); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if s.HostLayer {
			requireLinkNotTarget(t, dir, host)
		}

		id := s.seeded(t, dir)
		if _, out := s.exec(t, id, models.ExecSpec{Argv: s.Shell("readlink " + link)}); strings.TrimSpace(out) != host {
			t.Errorf("the link reads %q in the new sandbox, want %q", out, host)
		}
	})

	t.Run("SnapshotRefusesASourceThatIsRunning", func(t *testing.T) {
		source := s.running(t)
		err := s.Provider.Snapshot(t.Context(), source, s.EmptyDir(t))
		if err == nil {
			t.Fatal("Snapshot copied a running sandbox")
		}
		if !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), string(models.StateRunning)) {
			t.Errorf("the refusal is %q, and it must name the sandbox and its state", err)
		}
	})

	// A guest symlink in the writable layer is a host symlink, so the root daemon must refuse one that leads out (SHARD-300).
	t.Run("TrustProxyRefusesAGuestSymlinkOutOfTheLayer", func(t *testing.T) {
		if !s.HostLayer {
			t.Skip("the guest's writable layer is not a host directory")
		}

		host := t.TempDir()
		hostBundle := path.Join(host, "certs", "ca-certificates.crt")
		if err := os.MkdirAll(path.Dir(hostBundle), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hostBundle, []byte("the host's own\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		envs, ok := s.Provider.(environments)
		if !ok {
			t.Fatal("conformance: a HostLayer provider needs Environment")
		}

		spec := s.NewSpec(t)
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		env, err := envs.Environment(spec.ID)
		if err != nil {
			t.Fatalf("Environment: %v", err)
		}
		// The first plant proves the path is live, so the refusal below is the link and nothing else.
		if err := env.TrustProxy([]byte("conformance proxy CA\n")); err != nil {
			t.Fatalf("TrustProxy before the link: %v", err)
		}

		if err := s.Provider.Start(t.Context(), spec.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if status, out := s.exec(t, spec.ID, models.ExecSpec{Argv: s.Shell("rm -rf /etc/ssl && ln -s " + host + " /etc/ssl")}); status.Code != 0 {
			t.Fatalf("the guest's link exited %d: %s", status.Code, out)
		}
		if err := s.Provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if err := env.TrustProxy([]byte("conformance proxy CA\n")); err == nil {
			t.Error("TrustProxy followed a guest symlink out of the writable layer")
		}
		got, err := os.ReadFile(hostBundle)
		if err != nil {
			t.Fatalf("read the host's bundle: %v", err)
		}
		if string(got) != "the host's own\n" {
			t.Errorf("TrustProxy rewrote a host file through the guest's symlink: %q", got)
		}
	})

	// A created container holds the config.json of its create, so a grant before the first start reaches a process only through a new create (SHARD-526).
	t.Run("AGrantBeforeTheFirstStartReachesAProcess", func(t *testing.T) {
		if !s.HostLayer {
			t.Skip("the guest's writable layer is not a host directory")
		}

		envs, ok := s.Provider.(environments)
		if !ok {
			t.Fatal("conformance: a HostLayer provider needs Environment")
		}

		const ca, probed = "conformance late proxy CA", "conformance-probed"
		spec := s.NewSpec(t)
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		env, err := envs.Environment(spec.ID)
		if err != nil {
			t.Fatalf("Environment: %v", err)
		}
		if err := env.TrustProxy([]byte(ca + "\n")); err != nil {
			t.Fatalf("TrustProxy: %v", err)
		}
		if err := env.SetEnv("CONFORMANCE_LATE", "granted"); err != nil {
			t.Fatalf("SetEnv: %v", err)
		}

		if err := s.Provider.Start(t.Context(), spec.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		s.run(t, spec.ID, models.ProcessSpec{Name: "probe", Argv: s.Shell(`printf 'late=%s\n%s\n' "$CONFORMANCE_LATE" "$(tail -c 64 "$SSL_CERT_FILE")"; echo ` + probed)})

		out := s.awaitPrinted(t, spec.ID, "probe", probed)
		if !strings.Contains(out, "late=granted\n") {
			t.Errorf("the process never saw the variable granted before the first start:\n%s", out)
		}
		if !strings.Contains(out, ca) {
			t.Errorf("the process's CA bundle does not end in the proxy CA planted before the first start:\n%s", out)
		}
	})

	t.Run("Pause", func(t *testing.T) {
		id := s.running(t)
		err := s.Provider.Pause(t.Context(), id, s.EmptyDir(t))
		s.check(t, models.VerbPause, caps.Pause, err)
	})

	// A checkpoint is of a running sandbox on every substrate, so nothing else is a source for one.
	t.Run("PauseRefusesASandboxThatNeverStarted", func(t *testing.T) {
		if !caps.Pause {
			t.Skipf("%s does not support %s on this host", s.Provider.Name(), models.VerbPause)
		}

		spec := s.NewSpec(t)
		if err := s.Provider.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Provider.Pause(t.Context(), spec.ID, s.EmptyDir(t)); err == nil {
			t.Error("Pause of a sandbox that never started = nil, want a refusal")
		}
		if state := s.status(t, spec.ID).State; state != models.StateCreated {
			t.Errorf("the refused sandbox is %q, and a refusal leaves it %q", state, models.StateCreated)
		}
	})

	t.Run("Resume", func(t *testing.T) {
		id := s.running(t)
		dir := s.checkpointOf(t, id, caps.Pause)
		err := s.Provider.Resume(t.Context(), id, dir)
		s.check(t, models.VerbResume, caps.Resume, err)
	})

	t.Run("Fork", func(t *testing.T) {
		id := s.running(t)
		err := s.Provider.Fork(t.Context(), id, copyOf(s.NewSpec(t)))
		s.check(t, models.VerbFork, caps.Fork, err)
	})

	// A running source feeds as many forks as are asked of it, each from a capture of its own, and it runs on through them as it was (SHARD-457).
	t.Run("ManyForksOfARunningSource", func(t *testing.T) {
		if !caps.Fork {
			t.Skipf("%s does not support %s on this host", s.Provider.Name(), models.VerbFork)
		}

		source := s.running(t)
		before := s.status(t, source)
		if status, _ := s.exec(t, source, models.ExecSpec{Argv: s.Shell("echo source > " + s.scratch("conformance-fork"))}); status.Code != 0 {
			t.Fatalf("the write into the source exited %d", status.Code)
		}

		forks := make([]models.SandboxSpec, forkCount)
		for i := range forks {
			forks[i] = copyOf(s.NewSpec(t))
			if err := s.Provider.Fork(t.Context(), source, forks[i]); err != nil {
				t.Fatalf("fork %d of %d of one running source: %v", i+1, forkCount, err)
			}
			if after := s.status(t, source); after.State != models.StateRunning || after.PID != before.PID {
				t.Fatalf("the source is %+v after fork %d, want it running as pid %d, the runtime it had", after, i+1, before.PID)
			}
		}

		// Each fork is a sandbox of its own: its own process, and a command of its own that runs in it.
		pids := map[int]string{before.PID: source}
		for _, fork := range forks {
			status := s.status(t, fork.ID)
			if !status.Alive() || status.PID <= 0 {
				t.Fatalf("fork %s is %+v, want it running with a pid of its own", fork.ID, status)
			}
			if other, held := pids[status.PID]; held {
				t.Errorf("fork %s runs as pid %d, which %s already holds", fork.ID, status.PID, other)
			}
			pids[status.PID] = fork.ID

			if _, out := s.exec(t, fork.ID, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-fork"))}); !strings.Contains(out, "source") {
				t.Errorf("fork %s reads %q from the file the source wrote, want source", fork.ID, out)
			}
		}

		// A fork's own write stays in the fork: the capture copied the source's files, it does not share them.
		if !s.SharedScratch {
			if status, _ := s.exec(t, forks[0].ID, models.ExecSpec{Argv: s.Shell("echo fork > " + s.scratch("conformance-fork"))}); status.Code != 0 {
				t.Fatalf("the write into the first fork exited %d", status.Code)
			}
			if _, out := s.exec(t, source, models.ExecSpec{Argv: s.Shell("cat " + s.scratch("conformance-fork"))}); !strings.Contains(out, "source") {
				t.Errorf("the source reads %q after the first fork wrote its own copy, want source", out)
			}
		}

		// Only Stop ends a sandbox, and it ends the one it names.
		for _, fork := range forks {
			if err := s.Provider.Stop(t.Context(), fork.ID, stopGrace); err != nil {
				t.Fatalf("Stop fork %s: %v", fork.ID, err)
			}
		}
		if !s.status(t, source).Alive() {
			t.Error("the source went down with its forks")
		}
		if err := s.Provider.Stop(t.Context(), source, stopGrace); err != nil {
			t.Fatalf("Stop the source: %v", err)
		}
	})

	// Every fork of one running source wakes with the memory of its capture, so only a reseed on restore keeps two of them from drawing the same bytes (SHARD-266).
	t.Run("TheForksOfOneSourceDrawTheirOwnBytes", func(t *testing.T) {
		if !caps.Fork {
			t.Skipf("%s does not support %s on this host", s.Provider.Name(), models.VerbFork)
		}

		spec := s.NewSpec(t)
		// One vCPU, so every fork draws from the one per-cpu crng the capture saved.
		spec.Resources.VCPUs = 1
		source := s.start(t, spec)
		s.awaitUptime(t, source, s.ReseedWindow)

		drawn := map[string]string{}
		for range forkCount {
			fork := copyOf(s.NewSpec(t))
			if err := s.Provider.Fork(t.Context(), source, fork); err != nil {
				t.Fatalf("Fork: %v", err)
			}
			status, out := s.exec(t, fork.ID, models.ExecSpec{Argv: s.Shell("head -c 16 /dev/urandom | od -An -tx1")})
			draw := strings.Join(strings.Fields(out), "")
			if status.Code != 0 || len(draw) != 32 {
				t.Fatalf("the draw in fork %s exited %d with %q, want 16 bytes in hex", fork.ID, status.Code, out)
			}
			if other, held := drawn[draw]; held {
				t.Errorf("fork %s drew %s, the bytes fork %s drew first", fork.ID, draw, other)
			}
			drawn[draw] = fork.ID
			if err := s.Provider.Stop(t.Context(), fork.ID, stopGrace); err != nil {
				t.Fatalf("Stop fork %s: %v", fork.ID, err)
			}
		}
	})

	// A daemon restart opens a new provider over what the last one left; it runs last, since Reopen may close the first.
	t.Run("ANewProviderAdoptsARunningSandbox", func(t *testing.T) {
		id := s.running(t)
		s.run(t, id, models.ProcessSpec{Name: "tick", Argv: s.Shell("while true; do echo tick; sleep 0.2; done")})
		logged := s.awaitLog(t, id, "tick", 0)

		again := s.Reopen(t)
		status, err := again.Status(t.Context(), id)
		if err != nil {
			t.Fatalf("Status over the new provider: %v", err)
		}
		if !status.Alive() || status.PID <= 0 {
			t.Fatalf("the new provider sees %+v, want the sandbox running with its pid", status)
		}

		// The process's output must keep landing in its log, on the connection the new provider opened.
		s.awaitLog(t, id, "tick", logged)
		if tick, held := s.process(t, again, id, "tick"); !held || tick.State != models.ProcessRunning {
			t.Fatalf("the new provider reads %+v for the process the last one started, want it running", tick)
		}

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

		if err := again.StopProcess(t.Context(), id, "tick", 0); err != nil {
			t.Fatalf("StopProcess over the new provider: %v", err)
		}
		if err := again.Stop(t.Context(), id, stopGrace); err != nil {
			t.Fatalf("Stop over the new provider: %v", err)
		}
		if s.status(t, id).Alive() {
			t.Fatal("the first provider still sees the sandbox alive after the new one stopped it")
		}
	})
}

// awaitUptime returns once the sandbox's kernel has run for least, by its own clock.
func (s Subject) awaitUptime(t *testing.T, id string, least time.Duration) {
	t.Helper()

	if least <= 0 {
		return
	}
	status, out := s.exec(t, id, models.ExecSpec{Argv: s.Shell("cut -d' ' -f1 /proc/uptime")})
	up, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if status.Code != 0 || err != nil {
		t.Fatalf("the uptime read in sandbox %s exited %d with %q: %v", id, status.Code, out, err)
	}
	time.Sleep(least - time.Duration(up*float64(time.Second)))
}

// seeded creates and starts a fresh sandbox whose writable layer starts from the snapshot in dir.
func (s Subject) seeded(t *testing.T, dir string) string {
	t.Helper()

	spec := s.NewSpec(t)
	spec.Seed = dir

	return s.start(t, spec)
}

// requireLinkNotTarget fails unless the snapshot in dir holds a symlink to host and no copy of what host holds.
func requireLinkNotTarget(t *testing.T, dir, host string) {
	t.Helper()

	want, err := os.ReadFile(host)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	linked := false
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := root.Readlink(name)
			linked = linked || (err == nil && target == host)

			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		got, err := root.ReadFile(name)
		if err == nil && string(got) == string(want) {
			t.Errorf("the snapshot holds %s, a copy of the host file %s", filepath.Join(dir, name), host)
		}

		return err
	})
	if err != nil {
		t.Fatalf("walk the snapshot %s: %v", dir, err)
	}
	if !linked {
		t.Errorf("the snapshot %s holds no symlink to %s", dir, host)
	}
}

// copyOf is the spec the orchestrator hands Fork: the copy's id, name, lease and bounds.
func copyOf(spec models.SandboxSpec) models.SandboxSpec {
	return models.SandboxSpec{ID: spec.ID, Name: spec.Name, StateDir: spec.StateDir, Network: spec.Network, Resources: spec.Resources}
}

func (s Subject) scratch(name string) string {
	if s.Scratch == "" {
		return "/" + name
	}

	return path.Join(s.Scratch, name)
}

// ignoresTerm is a process that refuses SIGTERM, which is what proves grace; it prints readyMarker once it does.
func (s Subject) ignoresTerm(name string) models.ProcessSpec {
	return models.ProcessSpec{Name: name, Argv: s.Shell("trap '' TERM; echo " + readyMarker + "; while true; do sleep 0.1; done")}
}

// awaitPrinted blocks until the log of the named process holds marker, and returns the log.
func (s Subject) awaitPrinted(t *testing.T, id, name, marker string) string {
	t.Helper()

	path := s.logPath(t, id, name)
	deadline := time.Now().Add(waitSlack)
	for time.Now().Before(deadline) {
		out, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read the log %s: %v", path, err)
		}
		if strings.Contains(string(out), marker) {
			return string(out)
		}

		time.Sleep(readyPoll)
	}

	t.Fatalf("process %s of %s never printed %q within %s", name, id, marker, waitSlack)

	return ""
}

// awaitLog blocks until the log of the named process holds more than seen bytes, and returns how many it holds.
func (s Subject) awaitLog(t *testing.T, id, name string, seen int) int {
	t.Helper()

	path := s.logPath(t, id, name)
	deadline := time.Now().Add(waitSlack)
	for time.Now().Before(deadline) {
		out, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read the log %s: %v", path, err)
		}
		if len(out) > seen {
			return len(out)
		}

		time.Sleep(readyPoll)
	}

	t.Fatalf("the log of process %s of %s did not grow past %d bytes within %s", name, id, seen, waitSlack)

	return seen
}

func (s Subject) logPath(t *testing.T, id, name string) string {
	t.Helper()

	path, err := s.Provider.ProcessLogPath(id, name)
	if err != nil {
		t.Fatalf("ProcessLogPath: %v", err)
	}

	return path
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

// run starts one named process, which a required verb must do.
func (s Subject) run(t *testing.T, id string, spec models.ProcessSpec) {
	t.Helper()

	if err := s.Provider.StartProcess(t.Context(), id, spec); err != nil {
		t.Fatalf("StartProcess %s: %v", spec.Name, err)
	}
}

func (s Subject) processes(t *testing.T, provider models.Provider, id string) []models.ProcessReport {
	t.Helper()

	table, err := provider.Processes(t.Context(), id)
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}

	return table
}

// process is the named process's row in the table, and false when the table holds none.
func (s Subject) process(t *testing.T, provider models.Provider, id, name string) (models.ProcessReport, bool) {
	t.Helper()

	for _, report := range s.processes(t, provider, id) {
		if report.Name == name {
			return report, true
		}
	}

	return models.ProcessReport{}, false
}

// awaitProcess polls the table until the named process reads as wanted, since an end can reach the host after the verb that caused it returns.
func (s Subject) awaitProcess(t *testing.T, id, name string, want func(models.ProcessReport) bool) models.ProcessReport {
	t.Helper()

	var last models.ProcessReport
	deadline := time.Now().Add(waitSlack)
	for time.Now().Before(deadline) {
		report, held := s.process(t, s.Provider, id, name)
		if held && want(report) {
			return report
		}
		last = report

		time.Sleep(readyPoll)
	}

	t.Fatalf("process %s of %s never read as wanted within %s; it last read %+v", name, id, waitSlack, last)

	return last
}

func hasEnded(report models.ProcessReport) bool { return report.State.Ended() }

func isRunning(report models.ProcessReport) bool { return report.State == models.ProcessRunning }

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
func (s Subject) checkpointOf(t *testing.T, id string, canPause bool) string {
	t.Helper()

	dir := s.EmptyDir(t)
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
