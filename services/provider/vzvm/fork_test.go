package vzvm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/supervisor"
)

// A host that saves a VM pauses, resumes and forks a running sandbox (SHARD-463).
func TestCapabilitiesArePauseResumeAndFork(t *testing.T) {
	h := newHarness(t)
	want := models.Capabilities{Pause: true, Resume: true, Fork: true}
	if caps := h.provider.Capabilities(); caps != want {
		t.Fatalf("Capabilities = %+v, want %+v", caps, want)
	}
}

// A fork captures a running source live: the source runs on in the same shim, thawed on the stream it froze on, and its record never says paused (SHARD-463).
func TestAForkOfARunningSandboxLeavesTheSourceRunning(t *testing.T) {
	h, spec, pid := runningShim(t)
	fork := h.newSpec(t)
	mark(t, spec.StateDir, orderFile, freezesFile, controlsFile)
	mark(t, fork.StateDir, orderFile)
	before, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Fork(t.Context(), spec.ID, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	requireRunning(t, h.provider, spec.ID, pid, "the fork")
	status, err := h.provider.Status(t.Context(), fork.ID)
	if err != nil || !status.Alive() || status.PID == pid {
		t.Fatalf("Status of the fork = %+v, %v, want alive in a shim of its own", status, err)
	}
	if _, err := os.Stat(filepath.Join(fork.StateDir, vzvm.CaptureDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the capture after the fork: %v, want gone", err)
	}
	if got, want := lines(t, spec.StateDir, orderFile), []string{supervisor.KindFreeze, supervisor.KindThaw}; !slices.Equal(got, want) {
		t.Errorf("the guest of the source read %q, want %q", got, want)
	}
	if got, want := lines(t, spec.StateDir, freezesFile), []string{models.VerbFork}; !slices.Equal(got, want) {
		t.Errorf("the freezes of the source named %q, want %q", got, want)
	}
	// A save that keeps the stream thaws on it, so the guest takes no new control stream.
	if got := lines(t, spec.StateDir, controlsFile); len(got) != 0 {
		t.Errorf("the source took %d control streams over the fork, want none", len(got))
	}
	if got, want := lines(t, fork.StateDir, orderFile), []string{supervisor.KindReseed, supervisor.KindThaw}; !slices.Equal(got, want) {
		t.Errorf("the guest of the fork read %q, want %q", got, want)
	}
	after, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Errorf("the source's record after the fork = %s, want it unchanged from %s", after, before)
	}
	execOK(t, h.provider, spec.ID, "the fork")
	execOK(t, h.provider, fork.ID, "the fork")
}

// A save that resets every stream fails the thaw on the old one, so the source dials control again within the bound and thaws over it (SHARD-463).
func TestAForkWhoseSaveResetsTheStreamsThawsOnANewOne(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do echo tick; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	awaitLog(t, h.provider, spec.ID, 0)
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	pid := status.PID
	mark(t, spec.StateDir, orderFile, controlsFile, resetOnSaveFile)

	if err := h.provider.Fork(t.Context(), spec.ID, h.newSpec(t)); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	requireRunning(t, h.provider, spec.ID, pid, "the fork")
	if got, want := lines(t, spec.StateDir, orderFile), []string{supervisor.KindFreeze, supervisor.KindThaw}; !slices.Equal(got, want) {
		t.Errorf("the guest of the source read %q, want %q", got, want)
	}
	if got := lines(t, spec.StateDir, controlsFile); len(got) != 1 {
		t.Errorf("the source took %d control streams over the fork, want the one the redial put in", len(got))
	}
	execOK(t, h.provider, spec.ID, "the fork")
	logged, err := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	awaitLog(t, h.provider, spec.ID, len(logged))
}

// While a fork holds the source, through the save and the redial after it, the source reads running and refuses an exec by the fork's name (SHARD-463).
func TestASourceAForkHoldsReadsRunningAndRefusesAnExec(t *testing.T) {
	t.Run("the save", func(t *testing.T) {
		h, spec, pid := runningShim(t)
		mark(t, spec.StateDir, holdSaveFile)
		unmarkAtCleanup(t, spec.StateDir, holdSaveFile)
		forked := forkInBackground(t, h, spec.ID)

		awaitFile(t, filepath.Join(spec.StateDir, savingFile), forked)
		requireHeld(t, h, spec.ID, pid, "the save")
		unmark(t, spec.StateDir, holdSaveFile)
		if err := <-forked; err != nil {
			t.Fatalf("Fork: %v", err)
		}
		execOK(t, h.provider, spec.ID, "the fork")
	})

	t.Run("the redial", func(t *testing.T) {
		h, spec, pid := runningShim(t)
		t.Cleanup(vzvm.SetRedialGrace(30 * time.Second))
		mark(t, spec.StateDir, savesFile, resetOnSaveFile, holdDialsFile)
		unmarkAtCleanup(t, spec.StateDir, holdDialsFile)
		forked := forkInBackground(t, h, spec.ID)

		awaitFile(t, filepath.Join(spec.StateDir, savesFile), forked)
		// Every dial ends at once while the marker stays, so the whole second is the redial window.
		for end := time.Now().Add(time.Second); time.Now().Before(end); {
			requireHeld(t, h, spec.ID, pid, "the redial")
		}
		select {
		case err := <-forked:
			t.Fatalf("Fork returned %v while every dial ended at once, want it still dialing", err)
		default:
		}
		unmark(t, spec.StateDir, holdDialsFile)
		if err := <-forked; err != nil {
			t.Fatalf("Fork: %v", err)
		}
		execOK(t, h.provider, spec.ID, "the fork")
	})
}

// An exec past the hold check when a fork freezes is refused at its dial, and never waits on the save past its own deadline (SHARD-463).
func TestAnExecThatRacesAForkFreezeIsRefusedAtItsDial(t *testing.T) {
	h, spec, _ := runningShim(t)
	mark(t, spec.StateDir, holdSaveFile)
	unmarkAtCleanup(t, spec.StateDir, holdSaveFile)
	entered, release := make(chan struct{}), make(chan struct{})
	h.provider.HoldNextDir(entered, release)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	executed := make(chan error, 1)
	go func() {
		_, err := h.provider.Exec(ctx, spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}})
		executed <- err
	}()

	<-entered
	forked := forkInBackground(t, h, spec.ID)
	awaitFile(t, filepath.Join(spec.StateDir, savingFile), forked)
	close(release)
	select {
	case err := <-executed:
		want := fmt.Sprintf("sandbox %s could not run the command: a fork holds the sandbox frozen, and nothing starts in it until that ends: run the command again", spec.ID)
		if err == nil || err.Error() != want {
			t.Errorf("Exec that raced the freeze = %v, want %q", err, want)
		}
	case <-time.After(time.Second):
		t.Error("Exec that raced the freeze still waits on the save a second on, past its 250ms deadline")
	}
	unmark(t, spec.StateDir, holdSaveFile)
	if err := <-forked; err != nil {
		t.Fatalf("Fork: %v", err)
	}
}

// A source whose guest takes no control stream after the save fails the fork by name, leaves no fork, and still stops (SHARD-463).
func TestAForkWhoseSourceTakesNoStreamAgainSaysItIsFrozen(t *testing.T) {
	h, spec, fork, shim := severedFork(t)

	for _, name := range []string{"vm.json", vzvm.CaptureDir} {
		if _, err := os.Stat(filepath.Join(fork.StateDir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s of the fork after the failed fork: %v, want none", name, err)
		}
	}
	within(t, stopGrace+10*time.Second, "Stop of the frozen source", func() error {
		return h.provider.Stop(t.Context(), spec.ID, stopGrace)
	})
	awaitExit(t, shim)
}

// The follower dials on after a failed fork, and the stream the guest takes later thaws the source (SHARD-463).
func TestASourceAFailedForkLeftFrozenThawsOnALaterStream(t *testing.T) {
	h, spec, _, pid := severedFork(t)

	mark(t, spec.StateDir, thawedFile)
	unmark(t, spec.StateDir, holdDialsFile)
	// The order file has the thaw as the host sent it, and the guest refuses a command until it answers, so the exec waits for the answer.
	deadline := time.Now().Add(10 * time.Second)
	for len(lines(t, spec.StateDir, thawedFile)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the guest of the source answered no thaw within 10s, and read %q", lines(t, spec.StateDir, orderFile))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, want := lines(t, spec.StateDir, orderFile), []string{supervisor.KindFreeze, supervisor.KindThaw}; !slices.Equal(got, want) {
		t.Fatalf("the guest of the source read %q, want the fork's freeze and a thaw on a later stream", got)
	}
	requireRunning(t, h.provider, spec.ID, pid, "the thaw")
	execOK(t, h.provider, spec.ID, "the thaw")
}

// A stream that ends under the follower's thaw is dialed again and thaws the source there, with no lost state left for a later verb (SHARD-755).
func TestAThawWhoseStreamEndsThawsOnTheNextOne(t *testing.T) {
	h, spec, _, pid := severedFork(t)
	mark(t, spec.StateDir, thawedFile, cutThawFile)
	unmark(t, spec.StateDir, holdDialsFile)
	deadline := time.Now().Add(10 * time.Second)
	for len(lines(t, spec.StateDir, thawedFile)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the guest of the source answered no thaw within 10s, and read %q", lines(t, spec.StateDir, orderFile))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(spec.StateDir, cutThawFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the cut thaw marker = %v, want taken by the first thaw", err)
	}
	requireRunning(t, h.provider, spec.ID, pid, "the thaw")
	execOK(t, h.provider, spec.ID, "the thaw")
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop after a thaw whose stream ended: %v, want no lost state", err)
	}
}

// A save the VM refuses runs the source on, thawed, and leaves no capture and no fork behind (SHARD-463).
func TestAFailedSaveRunsTheSourceOn(t *testing.T) {
	h, spec, pid := runningShim(t)
	fork := h.newSpec(t)
	mark(t, spec.StateDir, orderFile, refuseSaveFile)

	err := h.provider.Fork(t.Context(), spec.ID, fork)
	if err == nil || !strings.Contains(err.Error(), "the vm refuses to save") {
		t.Fatalf("Fork over a refused save = %v, want the refusal", err)
	}
	requireRunning(t, h.provider, spec.ID, pid, "the refused save")
	for _, name := range []string{"vm.json", vzvm.CaptureDir} {
		if _, err := os.Stat(filepath.Join(fork.StateDir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s of the fork after the refused save: %v, want none", name, err)
		}
	}
	if got, want := lines(t, spec.StateDir, orderFile), []string{supervisor.KindFreeze, supervisor.KindThaw}; !slices.Equal(got, want) {
		t.Errorf("the guest of the source read %q, want %q", got, want)
	}
	execOK(t, h.provider, spec.ID, "the refused save")
}

// A fork id that already runs is refused before the source is frozen for nothing (SHARD-463).
func TestAForkOntoALiveIDIsRefusedBeforeAnyFreeze(t *testing.T) {
	h, spec, _ := runningShim(t)
	other, otherPID := runningShimOn(t, h)
	mark(t, spec.StateDir, orderFile)

	err := h.provider.Fork(t.Context(), spec.ID, other)
	if want := fmt.Sprintf("sandbox %s already exists", other.ID); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Fork onto a live id = %v, want %q", err, want)
	}
	if got := lines(t, spec.StateDir, orderFile); len(got) != 0 {
		t.Errorf("the guest of the source read %q, want no freeze", got)
	}
	requireRunning(t, h.provider, other.ID, otherPID, "the refused fork")
}

// A fork whose disk copy cannot fit is refused before the capture, so the source is never frozen or paused for it (SHARD-775).
func TestAForkThatCannotFitIsRefusedBeforeTheCapture(t *testing.T) {
	h, spec, pid := runningShim(t)
	fork := h.newSpec(t)
	mark(t, spec.StateDir, orderFile)
	// No host holds 8 TiB free, and a sparse disk takes none of it.
	if err := os.Truncate(filepath.Join(spec.StateDir, "disk.img"), 8<<40); err != nil {
		t.Fatal(err)
	}

	err := h.provider.Fork(t.Context(), spec.ID, fork)
	if room, ok := errors.AsType[*bundle.NoRoomError](err); !ok || !room.Copy {
		t.Fatalf("Fork = %v, want the copy refused for room", err)
	}
	if got := lines(t, spec.StateDir, orderFile); len(got) != 0 {
		t.Errorf("the guest of the source read %q, want no freeze", got)
	}
	requireRunning(t, h.provider, spec.ID, pid, "the refused fork")
	if _, err := os.Stat(filepath.Join(fork.StateDir, vzvm.CaptureDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the capture after the refusal: %v, want none", err)
	}
}

// A fork takes a running source only: a paused one is refused by name, and the fork's directory keeps no record and no capture (SHARD-463).
func TestAForkOfAPausedSandboxIsRefused(t *testing.T) {
	h, spec, _ := runningShim(t)
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	fork := h.newSpec(t)

	err := h.provider.Fork(t.Context(), spec.ID, fork)
	if err == nil || !strings.Contains(err.Error(), "fork takes a running sandbox") {
		t.Fatalf("Fork of a paused source = %v, want the refusal", err)
	}
	for _, name := range []string{"vm.json", vzvm.CaptureDir} {
		if _, err := os.Stat(filepath.Join(fork.StateDir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s of the fork after the refused fork: %v, want none", name, err)
		}
	}
}

// A daemon cut inside a capture leaves the source's VM paused and its root frozen under a running record: the next daemon resumes and thaws it, and its start's remove frees the half-made fork's id (SHARD-463).
func TestASourceACutForkLeftPausedRunsAgain(t *testing.T) {
	for _, cut := range []struct {
		name  string
		saved bool
	}{{"after the pause", false}, {"after the save", true}} {
		t.Run(cut.name, func(t *testing.T) {
			h, spec, pid := runningShim(t)
			fork := h.newSpec(t)
			mark(t, spec.StateDir, orderFile, freezesFile)
			if !cut.saved {
				mark(t, spec.StateDir, refuseSaveFile)
			}
			capture := filepath.Join(fork.StateDir, vzvm.CaptureDir)
			if err := h.provider.CaptureCut(t.Context(), spec.ID, capture); (err == nil) != cut.saved {
				t.Fatalf("CaptureCut = %v, want a save only %s", err, cut.name)
			}
			if !cut.saved {
				unmark(t, spec.StateDir, refuseSaveFile)
			}
			p := h.reopen(t)

			var status models.Status
			within(t, 5*time.Second, "Status after a cut fork", func() error {
				var err error
				status, err = p.Status(t.Context(), spec.ID)

				return err
			})
			if status.State != models.StateRunning || status.PID != pid {
				t.Fatalf("Status after a cut fork = %+v, want running as pid %d", status, pid)
			}
			_, info, err := vz.Adopt(t.Context(), filepath.Join(spec.StateDir, "shim.sock"))
			if err != nil || info.State != vz.StateRunning {
				t.Fatalf("the shim says %+v, %v; want its VM running", info, err)
			}
			if _, err := os.Stat(filepath.Join(spec.StateDir, frozenFile)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the guest's root is still frozen after the adopt: %v", err)
			}
			if r := readVM(t, spec.StateDir); r.Paused {
				t.Fatal("the record of the source says paused after a fork")
			}
			// The attach reseeds a guest a cut freeze left before it thaws, as it does after any cut pause (SHARD-375).
			if got, want := lines(t, spec.StateDir, orderFile), []string{supervisor.KindFreeze, supervisor.KindReseed, supervisor.KindThaw}; !slices.Equal(got, want) {
				t.Errorf("the guest read %q, want the cut fork's freeze, then the next daemon's reseed and thaw", got)
			}
			if got, want := lines(t, spec.StateDir, freezesFile), []string{models.VerbFork}; !slices.Equal(got, want) {
				t.Errorf("the freezes of the source named %q, want %q", got, want)
			}
			execOK(t, p, spec.ID, "the adopt")

			// A start of the daemon removes a fork that has no record yet, which frees its id for the next fork.
			if err := p.Remove(t.Context(), fork.ID); err != nil {
				t.Fatalf("Remove of the half-made fork: %v", err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the capture after the remove: %v, want gone", err)
			}
			if err := p.Fork(t.Context(), spec.ID, fork); err != nil {
				t.Fatalf("Fork into the freed id: %v", err)
			}
			execOK(t, p, fork.ID, "the fork into the freed id")
		})
	}
}

// A completed pause ends the shim, so the next daemon finds the sandbox paused with no VM to run again, and only a resume runs it (SHARD-463).
func TestACompletedPauseStaysPausedAcrossADaemonRestart(t *testing.T) {
	h, spec, _ := runningShim(t)
	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	p := h.reopen(t)

	status, err := p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status of a paused sandbox after the restart = %+v, %v, want stopped with no VM", status, err)
	}
	if r := readVM(t, spec.StateDir); !r.Paused {
		t.Fatal("the record lost the pause across the restart")
	}
	if err := p.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	status, err = p.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the resume = %+v, %v, want running", status, err)
	}
	execOK(t, p, spec.ID, "the resume")
}

// severedFork forks a running source whose guest takes no control stream after the save, so the redial runs out; it answers the source, the fork and the shim's pid.
func severedFork(t *testing.T) (*harness, models.SandboxSpec, models.SandboxSpec, int) {
	t.Helper()
	h, spec, pid := runningShim(t)
	t.Cleanup(vzvm.SetRedialGrace(500 * time.Millisecond))
	mark(t, spec.StateDir, orderFile, resetOnSaveFile, holdDialsFile)
	unmarkAtCleanup(t, spec.StateDir, holdDialsFile)
	fork := h.newSpec(t)

	err := h.provider.Fork(t.Context(), spec.ID, fork)
	if want := fmt.Sprintf("sandbox %s stays frozen after the fork", spec.ID); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Fork over a source that takes no stream = %v, want %q", err, want)
	}

	return h, spec, fork, pid
}

// forkInBackground forks the source into a new sandbox and answers the fork's error once it returns.
func forkInBackground(t *testing.T, h *harness, source string) <-chan error {
	t.Helper()
	fork := h.newSpec(t)
	forked := make(chan error, 1)
	go func() { forked <- h.provider.Fork(t.Context(), source, fork) }()

	return forked
}

// requireHeld proves a source a fork holds reads running at once, and refuses an exec, a signal and an app stop by the fork's name.
func requireHeld(t *testing.T, h *harness, id string, pid int, window string) {
	t.Helper()
	began := time.Now()
	status, err := h.provider.Status(t.Context(), id)
	if took := time.Since(began); err != nil || status.State != models.StateRunning || status.PID != pid || took >= time.Second {
		t.Fatalf("Status of the source in %s = %+v, %v after %s, want running as pid %d at once", window, status, err, took, pid)
	}
	_, err = h.provider.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	want := fmt.Sprintf("sandbox %s could not run the command: a fork holds the sandbox frozen, and nothing starts in it until that ends: run the command again", id)
	if err == nil || err.Error() != want {
		t.Fatalf("Exec on the source in %s = %v, want %q", window, err, want)
	}
	requireSendsRefused(t, h.provider, id, "fork", window)
}

// requireSendsRefused proves a signal and an app stop are refused at once by the name of the verb that holds the sandbox (SHARD-580).
func requireSendsRefused(t *testing.T, p models.Provider, id, verb, window string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := p.Signal(ctx, id, 1, "TERM")
	want := fmt.Sprintf("sandbox %s: a %s holds the sandbox frozen, so the signal was not sent: send it again once that ends", id, verb)
	if err == nil || err.Error() != want {
		t.Fatalf("Signal in %s = %v, want %q", window, err, want)
	}
	err = p.StopApp(ctx, id, false)
	want = fmt.Sprintf("sandbox %s: a %s holds the sandbox frozen, so the app stop was not sent: send it again once that ends", id, verb)
	if err == nil || err.Error() != want {
		t.Fatalf("StopApp in %s = %v, want %q", window, err, want)
	}
}

// requireRunning proves the sandbox runs on as the shim it had.
func requireRunning(t *testing.T, p models.Provider, id string, pid int, after string) {
	t.Helper()
	status, err := p.Status(t.Context(), id)
	if err != nil || status.State != models.StateRunning || status.PID != pid {
		t.Fatalf("Status of sandbox %s after %s = %+v, %v, want running as pid %d", id, after, status, err, pid)
	}
}

// execOK runs a command in the sandbox and fails the test unless it exits 0.
func execOK(t *testing.T, p models.Provider, id, after string) {
	t.Helper()
	exit, err := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err != nil || exit.Code != 0 {
		t.Fatalf("Exec on sandbox %s after %s = %+v, %v, want exit 0", id, after, exit, err)
	}
}

// awaitFile waits for the fake shim to write a line into path, and fails at once if the fork ends first.
func awaitFile(t *testing.T, path string, forked <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		read, err := os.ReadFile(path)
		if len(read) > 0 {
			return
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-forked:
			t.Fatalf("Fork returned %v before %s", err, filepath.Base(path))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 10s", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mark leaves each named marker for the fake shim in dir.
func mark(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func unmark(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

// unmarkAtCleanup lets go of a held fake shim before the cleanup stops its sandbox, should the test fail first.
func unmarkAtCleanup(t *testing.T, dir, name string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("remove the marker %s: %v", name, err)
		}
	})
}

// lines reads the fake shim's file of one line per event in dir.
func lines(t *testing.T, dir, name string) []string {
	t.Helper()
	read, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if len(read) == 0 {
		return nil
	}

	return strings.Split(strings.TrimSuffix(string(read), "\n"), "\n")
}
