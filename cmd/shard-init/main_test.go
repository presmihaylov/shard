package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// The supervisor is a process, so the tests re-execute this binary as both halves of the pair.
const (
	childPrefix    = "child:"
	roleEnv        = "SHARD_INIT_TEST_ROLE"
	roleSupervisor = "supervisor"
	roleOrphans    = "orphans"
	orphanCount    = 100
)

func TestMain(m *testing.M) {
	// The child inherits the supervisor environment, so its role comes from argv and wins here.
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], childPrefix) {
		os.Exit(runChild(strings.TrimPrefix(os.Args[1], childPrefix)))
	}

	switch os.Getenv(roleEnv) {
	case roleSupervisor:
		os.Exit(runSupervisor())
	case roleOrphans:
		spawnOrphans()
		os.Exit(runSupervisor())
	}

	os.Exit(m.Run())
}

func runSupervisor() int {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
		return 1
	}

	return 0
}

// spawnOrphans stands in for reparented grandchildren, which macOS cannot produce without a subreaper.
func spawnOrphans() {
	pids := make([]string, 0, orphanCount)
	for range orphanCount {
		// They outlive the handler installation on purpose, so no SIGCHLD arrives before it.
		pid, err := spawn([]string{os.Args[0], childPrefix + "sleep:300"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "spawn orphan:", err)
			os.Exit(1)
		}

		pids = append(pids, strconv.Itoa(pid))
	}

	fmt.Println("orphans " + strings.Join(pids, ","))
}

func runChild(spec string) int {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "exit":
		return atoi(arg)
	case "sigkill":
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	case "term":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		fmt.Println("ready")
		<-sigs
		return atoi(arg)
	case "sleep":
		time.Sleep(time.Duration(atoi(arg)) * time.Millisecond)
		return 0
	}

	fmt.Fprintln(os.Stderr, "unknown child role:", spec)
	return 2
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad number:", s)
		os.Exit(2)
	}

	return n
}

type supervisor struct {
	cmd      *exec.Cmd
	exitFile string
	out      *bufio.Reader
}

func startSupervisor(t *testing.T, role, child string) *supervisor {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	exitFile := filepath.Join(t.TempDir(), "exit.json")
	cmd := exec.Command(exe, "-exit-file", exitFile, "--", exe, childPrefix+child)
	cmd.Env = append(os.Environ(), roleEnv+"="+role)
	cmd.Stderr = os.Stderr

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe the supervisor stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}

	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	return &supervisor{cmd: cmd, exitFile: exitFile, out: bufio.NewReader(pipe)}
}

func (s *supervisor) line(t *testing.T) string {
	t.Helper()

	text, err := s.out.ReadString('\n')
	if err != nil {
		t.Fatalf("read a line from the supervisor: %v", err)
	}

	return strings.TrimSpace(text)
}

func (s *supervisor) awaitExitStatus(t *testing.T) models.ExitStatus {
	t.Helper()

	var status models.ExitStatus
	waitFor(t, 15*time.Second, "the exit status file", func() bool {
		blob, err := os.ReadFile(s.exitFile)
		if err != nil {
			return false
		}

		if err := json.Unmarshal(blob, &status); err != nil {
			t.Fatalf("the exit status file is not valid JSON: %v", err)
		}

		return true
	})

	return status
}

func (s *supervisor) alive(t *testing.T) bool {
	t.Helper()

	return s.cmd.Process.Signal(syscall.Signal(0)) == nil
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out after %s while waiting for %s", timeout, what)
}

func TestSupervisorOutlivesTheEntrypoint(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:7")

	status := super.awaitExitStatus(t)
	if status.Code != 7 || status.Signal != 0 {
		t.Errorf("exit status is %+v, want code 7 and signal 0", status)
	}

	// The exit file lands from the reaper, so give the supervisor a moment to exit if it means to.
	time.Sleep(200 * time.Millisecond)
	if !super.alive(t) {
		t.Error("the supervisor exited with the entrypoint, so a sandbox does not outlive it")
	}
}

func TestSignalledEntrypointRecordsItsSignal(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "sigkill:0")

	status := super.awaitExitStatus(t)
	if status.Signal != syscall.SIGKILL {
		t.Errorf("signal is %v, want SIGKILL", status.Signal)
	}
	if status.Code != 128+int(syscall.SIGKILL) {
		t.Errorf("code is %d, want %d", status.Code, 128+int(syscall.SIGKILL))
	}
}

func TestTermReachesTheEntrypoint(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "term:42")

	if got := super.line(t); got != "ready" {
		t.Fatalf("the entrypoint printed %q, want %q", got, "ready")
	}
	if err := super.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}

	status := super.awaitExitStatus(t)
	if status.Code != 42 {
		t.Errorf("exit status is %+v, want code 42, so SIGTERM did not reach the entrypoint", status)
	}
}

func TestNoZombiesAfterManyChildren(t *testing.T) {
	super := startSupervisor(t, roleOrphans, "sleep:600")

	pids, found := strings.CutPrefix(super.line(t), "orphans ")
	if !found {
		t.Fatal("the supervisor did not report the orphan pids")
	}

	super.awaitExitStatus(t)
	waitFor(t, 15*time.Second, "every orphan to be reaped", func() bool {
		out, err := exec.Command("ps", "-o", "pid=", "-p", pids).Output()
		if err != nil {
			// ps exits non-zero once no listed pid is left, which is the outcome under test.
			return true
		}

		return strings.TrimSpace(string(out)) == ""
	})
}

func TestRunRejectsBadArguments(t *testing.T) {
	cases := map[string][]string{
		"no exit file":  {"--", "/bin/true"},
		"no entrypoint": {"-exit-file", "/tmp/exit.json"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Errorf("run(%q) returned no error", args)
			}
		})
	}
}
