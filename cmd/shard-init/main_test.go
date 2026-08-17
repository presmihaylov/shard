package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// It mirrors main, exit code included, or a test would pin a code the real binary never returns.
func runSupervisor() int {
	err := run(os.Args[1:])
	if err == nil {
		return 0
	}

	fmt.Fprintln(os.Stderr, "shard-init:", err)

	return exitCodeFor(err)
}

// spawnOrphans stands in for reparented grandchildren, which macOS cannot produce without a subreaper.
func spawnOrphans() {
	pids := make([]string, 0, orphanCount)
	for range orphanCount {
		// They outlive the handler installation on purpose, so no SIGCHLD arrives before it.
		pid, err := startProcess([]string{os.Args[0], childPrefix + "sleep:300"})
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
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			fmt.Fprintln(os.Stderr, "kill self:", err)
			return 2
		}

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
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}

		// The supervisor never exits on its own, so Wait always reports the kill above.
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Errorf("wait for the supervisor: %v", err)
		}
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

	list, found := strings.CutPrefix(super.line(t), "orphans ")
	if !found {
		t.Fatal("the supervisor did not report the orphan pids")
	}

	pids := make([]int, 0, orphanCount)
	for field := range strings.SplitSeq(list, ",") {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("the supervisor reported %q as an orphan pid: %v", field, err)
		}

		pids = append(pids, pid)
	}

	super.awaitExitStatus(t)
	// A zombie still answers signal 0, so only ESRCH proves the reaper collected the pid.
	waitFor(t, 15*time.Second, "every orphan to be reaped", func() bool {
		for _, pid := range pids {
			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return false
			}
		}

		return true
	})
}

func TestSupervisorOutlivesALostExitStatus(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	// A missing parent directory is the lasting fault that no amount of retrying can get past.
	unwritable := filepath.Join(t.TempDir(), "no-such-dir", "exit.json")
	cmd := exec.Command(exe, "-exit-file", unwritable, "--", exe, childPrefix+"exit:0")
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)

	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("pipe the supervisor stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}

	defer func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}

		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Errorf("wait for the supervisor: %v", err)
		}
	}()

	// A sandbox outlives its entrypoint, so the lost status is reported and the supervisor stays up.
	reported := readLine(t, pipe)
	if !strings.Contains(reported, "write the exit status") {
		t.Errorf("the supervisor reported %q, want it to name the failed write", reported)
	}

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the supervisor died on a lost exit status: %v", err)
	}
}

func TestBrokenImageExitsSeparatelyFromABrokenSupervisor(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	exitFile := filepath.Join(t.TempDir(), "exit.json")
	cmd := exec.Command(exe, "-exit-file", exitFile, "--", "/no/such/entrypoint")
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("the supervisor returned %v, want it to exit non-zero", err)
	}

	if exit.ExitCode() != models.EntrypointNotStartedExitCode {
		t.Errorf("exit code is %d, want %d so a broken image is not read as a broken supervisor",
			exit.ExitCode(), models.EntrypointNotStartedExitCode)
	}
}

// readLine bounds the read, because a supervisor that says nothing is the failure under test here.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()

	lines := make(chan string, 1)
	go func() {
		text, err := bufio.NewReader(r).ReadString('\n')
		if err != nil {
			close(lines)

			return
		}

		lines <- text
	}()

	select {
	case text, ok := <-lines:
		if !ok {
			t.Fatal("the supervisor closed its stderr without reporting anything")
		}

		return strings.TrimSpace(text)
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor reported nothing within 15s")
	}

	return ""
}

func TestRunRejectsBadArguments(t *testing.T) {
	cases := map[string][]string{
		"no exit file":       {"--", "/bin/true"},
		"no entrypoint":      {"-exit-file", "/tmp/exit.json"},
		"relative exit file": {"-exit-file", "exit.json", "--", "/bin/true"},
		"exit file eats --":  {"-exit-file", "--", "/bin/true"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Errorf("run(%q) returned no error", args)
			}
		})
	}
}
