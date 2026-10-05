package reaper_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/reaper"
)

// leaderEnv runs this binary as a session leader whose child leads a group of its own, as a guest's entrypoint does.
const leaderEnv = "REAPER_TEST_LEADER"

func TestMain(m *testing.M) {
	if ran, code := reaper.Role(); ran {
		os.Exit(code)
	}
	if os.Getenv(leaderEnv) == "1" {
		if err := lead(); err != nil {
			fmt.Fprintln(os.Stderr, "lead:", err)
			os.Exit(1)
		}

		return
	}
	os.Exit(m.Run())
}

func lead() error {
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		return err
	}
	fmt.Println(child.Process.Pid)

	return child.Wait()
}

// session starts a leader in a session of its own, and returns it and its child.
func session(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	reaper.Require(t)

	leader := exec.Command(os.Args[0])
	leader.Env = append(os.Environ(), leaderEnv+"=1")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(-leader.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})
	line := make([]byte, 32)
	n, err := out.Read(line)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(string(line[:n-1]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(child, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})
	if pgid, err := syscall.Getpgid(child); err != nil || pgid != child {
		t.Fatalf("the child %d leads no group of its own: pgid %d, %v", child, pgid, err)
	}

	return leader, child
}

func notes(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "notes")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if err := reaper.Note(path, line); err != nil {
			t.Fatal(err)
		}
	}

	return path
}

func gone(t *testing.T, leader *exec.Cmd, start string, child int) {
	t.Helper()

	var exit *exec.ExitError
	if err := leader.Wait(); !errors.As(err, &exit) {
		t.Fatalf("the leader: %v, want killed", err)
	}
	left, err := reaper.Left(reaper.Marks{Sessions: map[int]string{leader.Process.Pid: start}})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("processes %v of the session outlive End", left)
	}
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the child %d of its own group outlives End: %v", child, err)
	}
}

// mark marks the session leader leads, and answers its notes line and the leader's start.
func mark(t *testing.T, leader *exec.Cmd) (string, string) {
	t.Helper()

	m, err := reaper.Session(leader.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}

	return m.String(), m.Sessions[leader.Process.Pid]
}

// End takes every group of a marked session, not only the one its leader leads.
func TestEndEndsEveryGroupOfAMarkedSession(t *testing.T) {
	leader, child := session(t)
	line, start := mark(t, leader)
	path := notes(t, line)

	if err := reaper.End(func() (reaper.Marks, error) { return reaper.Read(path) }); err != nil {
		t.Fatal(err)
	}
	gone(t, leader, start, child)
}

// A mark names one start, so a pid that a later process or session leader reuses is spared.
func TestEndSparesWhatReusesAMarkedPid(t *testing.T) {
	for _, kind := range []string{"p", "s"} {
		t.Run(kind, func(t *testing.T) {
			leader, child := session(t)
			path := notes(t, fmt.Sprintf("%s%d@0", kind, leader.Process.Pid))
			if err := reaper.End(func() (reaper.Marks, error) { return reaper.Read(path) }); err != nil {
				t.Fatal(err)
			}
			for _, pid := range []int{leader.Process.Pid, child} {
				if err := syscall.Kill(pid, 0); err != nil {
					t.Fatalf("End took %d, whose start the mark does not name: %v", pid, err)
				}
			}

			line, start := mark(t, leader)
			path = notes(t, line)
			if err := reaper.End(func() (reaper.Marks, error) { return reaper.Read(path) }); err != nil {
				t.Fatal(err)
			}
			gone(t, leader, start, child)
		})
	}
}

// The reaper ends what the index marks once the run lets it go, past a notes file its harness has removed.
func TestTheReaperEndsWhatTheIndexMarks(t *testing.T) {
	leader, child := session(t)
	line, start := mark(t, leader)
	dir := t.TempDir()
	index := filepath.Join(dir, "index")
	if err := os.WriteFile(index, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notes(t, line), filepath.Join(dir, "removed")} {
		if err := reaper.Note(index, path); err != nil {
			t.Fatal(err)
		}
	}

	reaped, err := reaper.Start(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := reaped(); err != nil {
		t.Fatal(err)
	}
	gone(t, leader, start, child)
}
