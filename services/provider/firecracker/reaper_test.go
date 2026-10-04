package firecracker_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeReaperEnv runs this binary as the reaper, which ends the vmm sessions of a test binary that dies mid-run and so runs no cleanup (SHARD-651).
const (
	fakeReaperEnv = "FIRECRACKER_FAKE_REAPER"
	launchReaper  = "launch"
	runReaper     = "run"
	// fakeHarnessesEnv names harnessesFile to the reaper.
	fakeHarnessesEnv = "FIRECRACKER_FAKE_HARNESSES"
)

// heldVMMEnv names the file TestHeldVMMOfAKilledRun writes its vmm session to.
const heldVMMEnv = "FIRECRACKER_FAKE_HELD_VMM"

// harnessesFile takes the sessions file of every harness this run opens.
var harnessesFile string

// startReaper starts the reaper through a launcher that exits, so a kill of this binary, its group or its process tree misses it; the func it returns ends the reaper and waits for its report.
func startReaper(harnesses string) (func() error, error) {
	alive, held, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	report, reported, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, alive.Close(), held.Close())
	}
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), fakeReaperEnv+"="+launchReaper, fakeHarnessesEnv+"="+harnesses)
	launcher.ExtraFiles = []*os.File{alive, reported}
	launcher.Stderr = os.Stderr
	// Once the launcher has run, only the reaper holds these two ends, so its exit closes them.
	if err := errors.Join(launcher.Run(), alive.Close(), reported.Close()); err != nil {
		return nil, errors.Join(err, held.Close(), report.Close())
	}

	return func() error {
		if err := held.Close(); err != nil {
			return errors.Join(err, report.Close())
		}
		out, err := io.ReadAll(report)
		if err := errors.Join(err, report.Close()); err != nil {
			return err
		}
		if len(out) > 0 {
			return errors.New(strings.TrimSpace(string(out)))
		}

		return nil
	}, nil
}

// launch starts the reaper in a session of its own, over fds 3 and 4, and exits, which leaves init its parent.
func launch() error {
	reaper := exec.Command(os.Args[0])
	reaper.Env = append(os.Environ(), fakeReaperEnv+"="+runReaper)
	reaper.ExtraFiles = []*os.File{os.NewFile(3, "alive"), os.NewFile(4, "report")}
	reaper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := reaper.Start(); err != nil {
		return err
	}

	return reaper.Process.Release()
}

// reap waits until the test binary closes fd 3, at the end of its run or by its death, then ends every vmm session a live harness noted, and writes any failure to fd 4.
func reap() error {
	// A process the reaper runs, such as ps, must not hold the report open past the reaper.
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	report := os.NewFile(4, "report")
	err := awaitAndEnd(os.NewFile(3, "alive"), os.Getenv(fakeHarnessesEnv))
	if err != nil {
		_, werr := fmt.Fprintln(report, err)
		err = errors.Join(err, werr)
	}

	return errors.Join(err, report.Close())
}

func awaitAndEnd(alive *os.File, harnesses string) error {
	// The test binary never writes to the pipe, so the copy returns only once the binary closes it or dies.
	if _, err := io.Copy(io.Discard, alive); err != nil {
		return fmt.Errorf("wait for the test binary: %w", err)
	}

	return end(func() (map[int]bool, error) { return liveSessions(harnesses) })
}

// liveSessions is every vmm session noted by a harness that still has its root.
func liveSessions(harnesses string) (map[int]bool, error) {
	blob, err := os.ReadFile(harnesses)
	if err != nil {
		return nil, err
	}
	sids := map[int]bool{}
	for path := range strings.SplitSeq(strings.TrimSpace(string(blob)), "\n") {
		if path == "" {
			continue
		}
		noted, err := sessionsIn(path)
		// A harness removes its root only after it has ended its sessions.
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		maps.Copy(sids, noted)
	}

	return sids, nil
}

// A test binary killed mid-run leaves no vmm session behind, even one whose vmm is stopped: the reaper ends it (SHARD-651).
func TestAKilledTestBinaryLeavesNoVMMSession(t *testing.T) {
	requireProcessTable(t)

	dir := t.TempDir()
	named := filepath.Join(dir, "sessions")
	logged, err := os.Create(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logged.Close(); err != nil {
			t.Error(err)
		}
	})
	child := exec.Command(os.Args[0], "-test.run=^TestHeldVMMOfAKilledRun$", "-test.count=1", "-test.v")
	child.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, fakeVMMEnv+"=") }), heldVMMEnv+"="+named)
	// A file, not a buffer: a guest that inherits it must not hold Wait open.
	child.Stdout = logged
	child.Stderr = logged
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() {
		if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
	})

	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		_, err := os.Stat(named)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			blob, rerr := os.ReadFile(logged.Name())
			if rerr != nil {
				t.Fatalf("the child ended (%v), and its log is unreadable: %v", err, rerr)
			}
			if err == nil && strings.Contains(string(blob), "--- SKIP") {
				t.Skipf("the child skipped:\n%s", blob)
			}
			t.Fatalf("the child ended before it named its vmm session: %v\n%s", err, blob)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the child named no vmm session within a minute")
		}
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := <-done; !errors.As(err, &exit) {
		t.Fatalf("the child after SIGKILL: %v, want killed", err)
	}
	sids, err := sessionsIn(named)
	if err != nil {
		t.Fatal(err)
	}
	// Ends what a red run leaves.
	t.Cleanup(func() { endSessions(t, named) })

	// The reaper may spend its own stopGrace on the sessions.
	for deadline := time.Now().Add(2 * stopGrace); ; time.Sleep(20 * time.Millisecond) {
		left, err := inSessions(sids)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes %v of the vmm sessions %v outlive their killed test binary by %s", left, sids, 2*stopGrace)
		}
	}
}

// TestHeldVMMOfAKilledRun runs only as the child of TestAKilledTestBinaryLeavesNoVMMSession, which kills it mid-run.
func TestHeldVMMOfAKilledRun(t *testing.T) {
	named := os.Getenv(heldVMMEnv)
	if named == "" {
		t.Skip("runs only as the child of TestAKilledTestBinaryLeavesNoVMMSession")
	}
	h := newHarness(t)
	_, pid := h.runLong(t)
	// A stopped vmm ends nothing of its own, as one a frozen-vmm test leaves.
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(h.root, sessionsFile))
	if err != nil {
		t.Fatal(err)
	}
	// The rename has the parent read the file only whole.
	if err := os.WriteFile(named+".tmp", blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(named+".tmp", named); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
	t.Error("no SIGKILL came within a minute")
}
