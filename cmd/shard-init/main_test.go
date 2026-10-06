package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
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
	// macOS has no /proc, so the files path runs the test binary, which answers it below as main does.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate the test binary:", err)
		os.Exit(1)
	}
	selfBinary = exe
	if len(os.Args) == 2 && os.Args[1] == supervisor.FilesMode {
		os.Exit(runFiles())
	}

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
		pid, err := startProcess(entrypoint{argv: []string{os.Args[0], childPrefix + "sleep:300"}, env: os.Environ()}, nil, false)
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
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return 0
	case "sleep":
		sleepWhileParented(time.Duration(atoi(arg)) * time.Millisecond)
		return 0
	case "run":
		// A run of MS milliseconds then exit CODE, so a test can make a run outlast the reset window.
		ms, code, _ := strings.Cut(arg, ":")
		time.Sleep(time.Duration(atoi(ms)) * time.Millisecond)
		return atoi(code)
	case "echo":
		// Stdin comes back on stdout and a marker on stderr, then the exit code the transport tests check.
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 2
		}
		fmt.Fprint(os.Stderr, "echo-err")
		return atoi(arg)
	case "say":
		fmt.Println(arg)
		return 0
	case "pwd":
		dir, err := os.Getwd()
		if err != nil {
			return 2
		}
		fmt.Println(dir)
		return 0
	case "spew":
		// A MiB of stdout, more than a pipe holds, then the marker file ARG, so a test sees the output never held it.
		if _, err := os.Stdout.Write(make([]byte, 1<<20)); err != nil {
			return 2
		}
		if err := os.WriteFile(arg, nil, 0o600); err != nil {
			return 2
		}
		time.Sleep(time.Minute)
		return 0
	}

	fmt.Fprintln(os.Stderr, "unknown child role:", spec)
	return 2
}

// sleepWhileParented ends early once the supervisor is gone, because a test's cleanup SIGKILLs it and would leave the sleep behind (SHARD-485).
func sleepWhileParented(d time.Duration) {
	// A supervisor killed before this ran already left pid 1 as the parent, and no test runs one as pid 1.
	parent := os.Getppid()
	deadline := time.After(d)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for parent != 1 && os.Getppid() == parent {
		select {
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad number:", s)
		os.Exit(2)
	}

	return n
}

type harness struct {
	cmd       *exec.Cmd
	exitFile  string
	readyFile string
	out       *bufio.Reader
	// waited records that a test collected the exit itself, so the cleanup does not wait twice.
	waited bool
}

// restart flags go before the entrypoint, and an empty child leaves none; the count rides the exit record.
func startSupervisor(t *testing.T, role, child string, restart ...string) *harness {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	dir := t.TempDir()
	exitFile := filepath.Join(dir, "exit.json")
	readyFile := filepath.Join(dir, "started")
	args := append(append([]string{"-ready-file", readyFile}, restart...), "--")
	if child != "" {
		args = append(args, exe, childPrefix+child)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), roleEnv+"="+role)
	cmd.Stderr = os.Stderr

	// shard-init reports the exit on fd 0, so the harness holds the write end as the supervisor's stdin.
	exitW, err := os.OpenFile(exitFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the exit channel: %v", err)
	}
	cmd.Stdin = exitW

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe the supervisor stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}
	// The supervisor holds its own copy of fd 0 now, so the harness drops its write end.
	if err := exitW.Close(); err != nil {
		t.Fatalf("close the exit channel write end: %v", err)
	}

	super := &harness{cmd: cmd, exitFile: exitFile, readyFile: readyFile, out: bufio.NewReader(pipe)}

	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}
		// Only a stop signal makes the supervisor exit, so most tests end with the kill above.
		if super.waited {
			return
		}

		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Errorf("wait for the supervisor: %v", err)
		}
	})

	return super
}

func (s *harness) line(t *testing.T) string {
	t.Helper()

	text, err := s.out.ReadString('\n')
	if err != nil {
		t.Fatalf("read a line from the supervisor: %v", err)
	}

	return strings.TrimSpace(text)
}

func (s *harness) awaitExitStatus(t *testing.T) models.ExitStatus {
	t.Helper()

	var status models.ExitStatus
	waitFor(t, 15*time.Second, "the exit status on fd 0", func() bool {
		exit, found := readFramedExit(t, s.exitFile)
		if !found {
			return false
		}

		status = exit

		return true
	})

	return status
}

// readFramedExit reads the last complete record shard-init framed onto fd 0, mirroring the host reader.
func readFramedExit(t *testing.T, path string) (models.ExitStatus, bool) {
	t.Helper()

	report, found := readFramedReport(t, path)
	if !found {
		return models.ExitStatus{}, false
	}

	return models.ExitStatus{Code: report.Code, Signal: report.Signal}, true
}

// readFramedReport answers the last complete exit record, with the restart count it carries.
func readFramedReport(t *testing.T, path string) (models.ExitReport, bool) {
	t.Helper()

	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return models.ExitReport{}, false
	}
	if err != nil {
		t.Fatalf("read the exit channel: %v", err)
	}

	end := bytes.LastIndexByte(blob, '\n')
	if end < 0 {
		return models.ExitReport{}, false
	}

	var line []byte
	for candidate := range bytes.SplitSeq(blob[:end+1], []byte{'\n'}) {
		if trimmed := bytes.TrimSpace(candidate); len(trimmed) > 0 {
			line = trimmed
		}
	}
	if line == nil {
		return models.ExitReport{}, false
	}

	var report models.ExitReport
	if err := json.Unmarshal(line, &report); err != nil {
		t.Fatalf("the exit record is not valid JSON: %v", err)
	}
	if report.Kind != models.ExitReportKind {
		return models.ExitReport{}, false
	}

	return report, true
}

// awaitRestartCount waits until the count on the exit record says what the test wants of it.
func (s *harness) awaitRestartCount(t *testing.T, want func(models.RestartCount) bool) models.RestartCount {
	t.Helper()

	var count models.RestartCount
	waitFor(t, 15*time.Second, "the restart count", func() bool {
		report, found := readFramedReport(t, s.exitFile)
		if !found {
			return false
		}
		count = report.Restarts

		return want(count)
	})

	return count
}

func (s *harness) awaitReady(t *testing.T) {
	t.Helper()

	waitFor(t, 15*time.Second, "the handshake", func() bool {
		_, err := os.Stat(s.readyFile)

		return err == nil
	})
}

// awaitExit collects the supervisor's own exit, which nothing but a stop signal produces.
func (s *harness) awaitExit(t *testing.T) {
	t.Helper()

	s.waited = true

	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the supervisor ended with %v, want a clean exit", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the supervisor stayed up after a stop signal, so a stop can only ever kill it")
	}
}

func (s *harness) alive(t *testing.T) bool {
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
	if status.Signal != int(syscall.SIGKILL) {
		t.Errorf("signal is %v, want SIGKILL", status.Signal)
	}
	if status.Code != 128+int(syscall.SIGKILL) {
		t.Errorf("code is %d, want %d", status.Code, 128+int(syscall.SIGKILL))
	}
}

// TERM must reach the entrypoint, and a stop must not have to wait out its grace afterwards.
func TestTermEndsTheSupervisorOnceTheEntrypointIsReaped(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "term:42")

	if got := super.line(t); got != "ready" {
		t.Fatalf("the entrypoint printed %q, want %q", got, "ready")
	}
	if err := super.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}

	super.awaitExit(t)

	if status := super.awaitExitStatus(t); status.Code != 42 {
		t.Errorf("exit status is %+v, want code 42: SIGTERM must reach the entrypoint and be reaped", status)
	}
}

// The common case: the entrypoint finished long ago and the sandbox stayed up until the stop.
func TestTermEndsASupervisorWhoseEntrypointAlreadyExited(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:0")
	super.awaitExitStatus(t)

	if err := super.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}

	super.awaitExit(t)
}

// SHARD-764: runc made the work directory under the daemon's umask 0077, so a user other than root could not enter it.
func TestTheSupervisorMakesTheWorkDirectory0755AndStartsTheEntrypointThere(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	root := t.TempDir()
	workDir := filepath.Join(root, "work", "deep")

	super := startSupervisor(t, roleSupervisor, "pwd", "-workdir", workDir)

	got := super.line(t)
	want, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("resolve %s: %v", workDir, err)
	}
	if got != want {
		t.Errorf("the entrypoint ran in %q, want %q", got, want)
	}
	for _, dir := range []string{filepath.Dir(workDir), workDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s is %o, want 755", dir, info.Mode().Perm())
		}
	}
}

// With no command the supervisor runs alone: it is ready at once and stays up, as a sandbox outlives any entrypoint.
func TestASupervisorWithNoEntrypointIsReadyAndStaysUp(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "")

	super.awaitReady(t)
	time.Sleep(200 * time.Millisecond)
	if !super.alive(t) {
		t.Error("the supervisor exited with no entrypoint, so the sandbox did not stay up")
	}
}

// Nothing ran, so a stop ends the supervisor cleanly and leaves no exit record for the host to read.
func TestTermEndsASupervisorWithNoEntrypoint(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "")
	super.awaitReady(t)

	if err := super.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}
	super.awaitExit(t)

	if status, found := readFramedExit(t, super.exitFile); found {
		t.Errorf("the supervisor reported the exit %+v, but nothing ran", status)
	}
}

func TestOnFailureStartsTheEntrypointAgainUntilTheRetriesAreSpent(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:1", "-restart", "on-failure", "-retries", "2", "-backoff", "20ms")

	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.GaveUp })
	if count.Count != 2 || count.LastAt.IsZero() {
		t.Errorf("the count is %+v, want 2 starts again with a time on the last", count)
	}
	if status := super.awaitExitStatus(t); status.Code != 1 {
		t.Errorf("exit status is %+v, want code 1 from the last run", status)
	}
	if !super.alive(t) {
		t.Error("the supervisor exited at the cap, so a sandbox does not outlive a give-up")
	}
}

// always never gives up, so a clean exit starts the entrypoint again without end.
func TestAlwaysNeverGivesUpAfterACleanExit(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:0", "-restart", "always", "-backoff", "1ms")

	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Count >= 10 })
	if count.GaveUp {
		t.Errorf("the count is %+v, want no give-up under always", count)
	}
}

// on-failure with no retries is unlimited, so a failing entrypoint starts again without end.
func TestOnFailureIsUnlimitedByDefault(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:1", "-restart", "on-failure", "-backoff", "1ms")

	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Count >= 10 })
	if count.GaveUp {
		t.Errorf("the count is %+v, want no give-up while the retries are unlimited", count)
	}
}

// A run that lasts the reset window clears the count, so a slow crash loop never spends a finite cap.
func TestAHealthyRunClearsTheRestartCount(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "run:60:1", "-restart", "on-failure", "-retries", "1", "-backoff", "1ms", "-restart-reset", "40ms")

	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Count >= 1 })
	first := count.LastAt
	// A give-up here would mean the reset did nothing, so a start again past the cap is the proof.
	count = super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.GaveUp || c.LastAt.After(first) })
	if count.GaveUp {
		t.Fatalf("the supervisor gave up at %+v, want a healthy run to clear the count first", count)
	}
	if count.Count != 1 {
		t.Errorf("the count is %+v, want it reset to 0 then back to 1 on each start again", count)
	}
}

func TestOnFailureLeavesACleanExitAlone(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:0", "-restart", "on-failure", "-retries", "2", "-backoff", "20ms")
	super.awaitExitStatus(t)

	// The first start again would land within the backoff, so a quiet wait past it proves the point.
	time.Sleep(200 * time.Millisecond)
	if count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Ended }); count.Count != 0 || count.GaveUp {
		t.Errorf("the count is %+v, want the app ended with no start again", count)
	}
}

// A run waits for the end, so every policy writes it once no start again follows the last exit.
func TestEveryPolicyEndsTheApp(t *testing.T) {
	cases := map[string]struct {
		child   string
		restart []string
		want    models.RestartCount
	}{
		"no policy":           {"exit:3", nil, models.RestartCount{Ended: true}},
		"on-failure, success": {"exit:0", []string{"-restart", "on-failure", "-backoff", "1ms"}, models.RestartCount{Ended: true}},
		"on-failure, give-up": {"exit:4", []string{"-restart", "on-failure", "-retries", "2", "-backoff", "1ms"}, models.RestartCount{Count: 2, GaveUp: true, Ended: true}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			super := startSupervisor(t, roleSupervisor, c.child, c.restart...)

			count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Ended })
			count.LastAt = time.Time{}
			if count != c.want {
				t.Errorf("the count is %+v, want %+v", count, c.want)
			}
			if !super.alive(t) {
				t.Error("the supervisor exited with the app, so the sandbox did not outlive it")
			}
		})
	}
}

// USR1 is a run's Ctrl+C: it terms the app, cancels every start again, and leaves the sandbox up.
func TestUSR1TermsTheAppAndCancelsItsRestarts(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "term:7", "-restart", "always", "-backoff", "1ms")
	if got := super.line(t); got != "ready" {
		t.Fatalf("the app printed %q, want ready", got)
	}

	if err := super.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}

	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Ended })
	if count.Count != 0 || count.GaveUp {
		t.Errorf("the count is %+v, want the app ended with no start again", count)
	}
	if status := super.awaitExitStatus(t); status.Code != 7 {
		t.Errorf("exit status is %+v, want the app's own 7 on its TERM", status)
	}
	if !super.alive(t) {
		t.Error("the supervisor exited on USR1, want the sandbox up with only shard-init")
	}
}

// An app that ignores TERM outlives USR1, and USR2 kills it.
func TestUSR2KillsAnAppThatIgnoresTerm(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "ignoreterm", "-restart", "always", "-backoff", "1ms")
	if got := super.line(t); got != "ready" {
		t.Fatalf("the app printed %q, want ready", got)
	}

	if err := super.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if report, found := readFramedReport(t, super.exitFile); found {
		t.Fatalf("the app ended on a TERM it ignores: %+v", report)
	}

	if err := super.cmd.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}
	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Ended })
	if count.Count != 0 {
		t.Errorf("the count is %+v, want no start again after the kill", count)
	}
	if status := super.awaitExitStatus(t); status.Signal != int(syscall.SIGKILL) {
		t.Errorf("exit status is %+v, want SIGKILL", status)
	}
}

// A stop in the backoff wait has no app to signal, so it ends the app where it is.
func TestUSR1InTheBackoffWaitEndsTheApp(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:1", "-restart", "on-failure", "-backoff", "10s")
	super.awaitExitStatus(t)

	if err := super.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}

	count := super.awaitRestartCount(t, func(c models.RestartCount) bool { return c.Ended })
	if count.Count != 0 || count.GaveUp {
		t.Errorf("the count is %+v, want the app ended before its start again", count)
	}
}

// A sandbox with no app has nothing to stop, and the supervisor stays up.
func TestUSR1WithNoAppChangesNothing(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "")
	super.awaitReady(t)

	if err := super.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !super.alive(t) {
		t.Error("the supervisor exited on USR1 with no app")
	}
	if report, found := readFramedReport(t, super.exitFile); found {
		t.Errorf("a sandbox with no app wrote an end: %+v", report)
	}
}

func TestTermDuringTheBackoffEndsTheSupervisorAtOnce(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "exit:1", "-restart", "on-failure", "-retries", "5", "-backoff", "10s")
	super.awaitExitStatus(t)

	if err := super.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}

	super.awaitExit(t)
}

func TestTheBackoffDoublesUpToTheCap(t *testing.T) {
	policy := restartPolicy{backoff: time.Second}
	cases := map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 5: 32 * time.Second, 6: 60 * time.Second, 100: 60 * time.Second}

	for started, want := range cases {
		if got := policy.wait(started); got != want {
			t.Errorf("wait(%d) = %s, want %s", started, got, want)
		}
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

// darwin can drop a SIGCHLD under load, so a death no signal announced must still reach the host (SHARD-481).
func TestADeathNoSignalAnnouncedIsStillReaped(t *testing.T) {
	report := memoryReporter{exits: make(chan models.ExitStatus, 1), ooms: make(chan struct{}, 1)}
	g := newGuest(report, restartPolicy{policy: models.RestartNo})
	signal.Stop(g.childDeaths)
	if err := g.launch(entrypoint{argv: []string{os.Args[0], childPrefix + "exit:7"}, env: os.Environ()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.supervise() }()

	select {
	case exit := <-report.exits:
		if exit.Code != 7 {
			t.Fatalf("exit = %+v, want code 7", exit)
		}
	case <-time.After(5 * reapEvery):
		t.Fatalf("no exit within %s of a death that no SIGCHLD announced", 5*reapEvery)
	}
	// Every guest in this process reaps any child, so this one must end before the next test forks.
	g.stopSignals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest did not end on a stop with nothing to forward to")
	}
}

func TestSupervisorOutlivesALostExitStatus(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	// A read-only fd 0 is a write end the supervisor can never report on, the lasting fault under test.
	dir := t.TempDir()
	readOnly, err := os.OpenFile(filepath.Join(dir, "exit.json"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatalf("open the exit channel read-only: %v", err)
	}
	defer func() {
		if err := readOnly.Close(); err != nil {
			t.Errorf("close the exit channel: %v", err)
		}
	}()

	cmd := exec.Command(exe, "-ready-file", filepath.Join(dir, "started"), "--", exe, childPrefix+"exit:0")
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)
	cmd.Stdin = readOnly

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
	if !strings.Contains(reported, "report the exit record on fd 0") {
		t.Errorf("the supervisor reported %q, want it to name the failed report", reported)
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

	dir := t.TempDir()
	readyFile := filepath.Join(dir, "started")
	cmd := exec.Command(exe, "-ready-file", readyFile, "--", "/no/such/entrypoint")
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("the supervisor returned %v, want it to exit non-zero", err)
	}

	if exit.ExitCode() != models.EntrypointNotStartedExitCode {
		t.Errorf("exit code is %d, want %d so a broken image is not read as a broken supervisor",
			exit.ExitCode(), models.EntrypointNotStartedExitCode)
	}

	// The handshake is the host's only proof, so an entrypoint that never ran must leave none.
	if _, err := os.Stat(readyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s returned %v, want the handshake to be absent", readyFile, err)
	}
}

// The host builds the refusal from the errno alone, so the record must carry the one execve answered.
func TestAnEntrypointThatCannotRunLeavesItsErrnoOnFd0(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write a file with no execute bit: %v", err)
	}

	for name, c := range map[string]struct {
		argv0 string
		errno syscall.Errno
	}{
		"a path that is not there": {"/no/such/entrypoint", syscall.ENOENT},
		"a name on no PATH entry":  {"no-such-entrypoint", syscall.ENOENT},
		"a file with no exec bit":  {plain, syscall.EACCES},
	} {
		t.Run(name, func(t *testing.T) {
			exitFile := filepath.Join(t.TempDir(), "exit.json")
			exitW, err := os.OpenFile(exitFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatalf("open the exit channel: %v", err)
			}
			defer func() {
				if err := exitW.Close(); err != nil {
					t.Errorf("close the exit channel: %v", err)
				}
			}()

			cmd := exec.Command(exe, "-ready-file", filepath.Join(dir, "started"), "--", c.argv0)
			cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)
			cmd.Stdin = exitW
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) {
				t.Fatalf("the supervisor returned %v, want it to exit non-zero", err)
			}

			blob, err := os.ReadFile(exitFile)
			if err != nil {
				t.Fatalf("read the exit channel: %v", err)
			}
			var report models.ExitReport
			if err := json.Unmarshal(bytes.TrimSpace(blob), &report); err != nil {
				t.Fatalf("decode the record %q: %v", blob, err)
			}
			if report.Kind != models.NotStartedReportKind || report.Errno != int(c.errno) {
				t.Errorf("the record is %+v, want kind %s with errno %d", report, models.NotStartedReportKind, c.errno)
			}
		})
	}
}

// A fork or credential failure is the supervisor's, so it must never read to the host as a command that cannot run.
func TestExecErrnoIsZeroForAFailureThatIsNotTheCommand(t *testing.T) {
	for name, err := range map[string]error{
		"a setup failure":     fmt.Errorf("set up: %w", syscall.ENOENT),
		"a fork failure":      unrunnable{fmt.Errorf("fork: %w", syscall.EAGAIN)},
		"a credential denial": unrunnable{fmt.Errorf("setuid: %w", syscall.EPERM)},
	} {
		if got := execErrno(err); got != 0 {
			t.Errorf("execErrno(%s) = %v, want zero", name, got)
		}
	}
	if got := execErrno(unrunnable{fmt.Errorf("lookup: %w", exec.ErrNotFound)}); got != syscall.ENOENT {
		t.Errorf("execErrno of a lookup miss = %v, want ENOENT", got)
	}
}

// The handshake is what makes a started sandbox distinguishable from one whose entrypoint died on
// the way in. runsc start unblocks the task and reads nothing back.
func TestTheSupervisorReportsThatTheEntrypointStarted(t *testing.T) {
	super := startSupervisor(t, roleSupervisor, "sleep:5000")

	super.awaitReady(t)
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
	const readyFlag, readyPath = "-ready-file", "/tmp/started"

	cases := map[string][]string{
		"no ready file":          {"--", "/bin/true"},
		"relative ready file":    {readyFlag, "started", "--", "/bin/true"},
		"ready file eats --":     {readyFlag, "--", "/bin/true"},
		"user with no gid":       {readyFlag, readyPath, "-user", "1000", "--", "/bin/true"},
		"user with a name":       {readyFlag, readyPath, "-user", "nobody:nobody", "--", "/bin/true"},
		"user with no ids":       {readyFlag, readyPath, "-user", ":", "--", "/bin/true"},
		"user with an extra":     {readyFlag, readyPath, "-user", "1000:1000:10", "--", "/bin/true"},
		"an id past 32 bits":     {readyFlag, readyPath, "-user", "4294967296:0", "--", "/bin/true"},
		"a negative id":          {readyFlag, readyPath, "-user", "-1:0", "--", "/bin/true"},
		"unknown policy":         {readyFlag, readyPath, "-restart", "unless-stopped", "--", "/bin/true"},
		"negative retries":       {readyFlag, readyPath, "-restart", "on-failure", "-retries", "-1", "--", "/bin/true"},
		"zero backoff":           {readyFlag, readyPath, "-restart", "always", "-backoff", "0s", "--", "/bin/true"},
		"root with base":         {"-transport", "unix:/tmp/x", "-root", "/dev/vda", "-base", "/dev/vdb", "-overlay", "/dev/vdc"},
		"base with no overlay":   {"-transport", "unix:/tmp/x", "-base", "/dev/vda"},
		"overlay with no base":   {"-transport", "unix:/tmp/x", "-overlay", "/dev/vdb"},
		"root without transport": {readyFlag, readyPath, "-root", "/dev/vda", "--", "/bin/true"},
		"base without transport": {readyFlag, readyPath, "-base", "/dev/vda", "-overlay", "/dev/vdb", "--", "/bin/true"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Errorf("run(%q) returned no error", args)
			}
		})
	}
}

// The error pipe of a fork reads EOF on a death before the exec too, so the flag the kernel clears at the exec is the proof it ran (SHARD-505).
func TestStatExeced(t *testing.T) {
	line := func(name string, flags uint64) string {
		return fmt.Sprintf("42 (%s) Z 1 42 42 0 -1 %d 0 0 0 0 0 0 0 0 20 0 1 0", name, flags)
	}
	cases := map[string]struct {
		stat string
		want bool
	}{
		"an exec":                       {stat: line("sleep", 0x400000), want: true},
		"a death before the exec":       {stat: line("shard-init", 0x400000|pfForkNoExec), want: false},
		"a name that holds a stat line": {stat: line("a) Z 1 42 42 0 -1 64 (b", 0x400000), want: true},
		"a name that holds a ')'":       {stat: line("x) S 1", 0x400000|pfForkNoExec), want: false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := statExeced(c.stat)
			if err != nil {
				t.Fatalf("statExeced(%q): %v", c.stat, err)
			}
			if got != c.want {
				t.Errorf("statExeced(%q) = %t, want %t", c.stat, got, c.want)
			}
		})
	}
}

func TestStatExecedRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no name":          "42 S 1 42 42 0 -1 0",
		"no flags":         "42 (sleep) S 1 42 42 0",
		"unreadable flags": "42 (sleep) S 1 42 42 0 -1 x",
	}

	for name, stat := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := statExeced(stat); err == nil {
				t.Errorf("statExeced(%q) returned no error", stat)
			}
		})
	}
}

// The host resolves the name, so the supervisor only ever reads ids off the flags.
func TestParseCredential(t *testing.T) {
	cases := map[string]struct {
		user   string
		groups string
		want   *syscall.Credential
	}{
		"none":     {user: "", want: nil},
		"root":     {user: "0:0", groups: "0", want: &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0}}},
		"a pair":   {user: "1000:2000", groups: "2000", want: &syscall.Credential{Uid: 1000, Gid: 2000, Groups: []uint32{2000}}},
		"a set":    {user: "1000:1000", groups: "1000,10,999", want: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000, 10, 999}}},
		"the most": {user: "4294967295:4294967295", groups: "4294967295", want: &syscall.Credential{Uid: 4294967295, Gid: 4294967295, Groups: []uint32{4294967295}}},
		// An image that says nothing about the user drops the entrypoint to no supplementary group at all.
		"no groups": {user: "1000:1000", groups: "", want: &syscall.Credential{Uid: 1000, Gid: 1000}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseCredential(c.user, c.groups)
			if err != nil {
				t.Fatalf("parseCredential(%q, %q): %v", c.user, c.groups, err)
			}
			if c.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want no credential", got)
				}

				return
			}
			if got == nil {
				t.Fatalf("got no credential, want %+v", c.want)
			}
			if got.Uid != c.want.Uid || got.Gid != c.want.Gid || !slices.Equal(got.Groups, c.want.Groups) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
			// NoSetGroups false is what makes an empty set a setgroups(0, NULL) and not an inheritance.
			if got.NoSetGroups {
				t.Error("the credential skips setgroups, so the entrypoint keeps the group set of PID 1")
			}
		})
	}
}

func TestParseCredentialRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]struct{ user, groups string }{
		"no gid":             {user: "1000"},
		"an unreadable uid":  {user: "build:1000"},
		"an unreadable gid":  {user: "1000:build"},
		"an unreadable set":  {user: "1000:1000", groups: "1000,wheel"},
		"an empty field":     {user: "1000:1000", groups: "1000,"},
		"a set with no user": {groups: "1000"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCredential(c.user, c.groups); err == nil {
				t.Errorf("parseCredential(%q, %q) returned no error", c.user, c.groups)
			}
		})
	}
}

// The permitted set is the ceiling config.json granted the supervisor, and the entrypoint is handed
// exactly it: a uid change away from root clears the set the sandbox spec advertises.
func TestPermittedMask(t *testing.T) {
	const status = "Name:\tshard-init\nCapInh:\t00000000a80425fb\nCapPrm:\t00000000a80425fb\nCapEff:\t0000000000000000\n"

	mask, err := permittedMask(status)
	if err != nil {
		t.Fatalf("permittedMask: %v", err)
	}
	if want := uint64(0xa80425fb); mask != want {
		t.Errorf("got mask %#x, want %#x", mask, want)
	}

	// CAP_NET_BIND_SERVICE is 10, and it is the one a --user entrypoint most visibly loses without this.
	if got := capabilitiesIn(mask); !slices.Contains(got, uintptr(10)) {
		t.Errorf("got the capabilities %v, want CAP_NET_BIND_SERVICE among them", got)
	}
	if got := capabilitiesIn(0); got != nil {
		t.Errorf("an empty set gave %v, want none", got)
	}
}

func TestPermittedMaskRefusesAStatusItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no field":     "Name:\tshard-init\n",
		"not a number": "CapPrm:\tnot-a-mask\n",
	}

	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := permittedMask(status); err == nil {
				t.Errorf("permittedMask(%q) returned no error", status)
			}
		})
	}
}

// A child that keeps the supervisor's own ids loses nothing, so it needs no ambient set at all.
func TestNoAmbientSetWithoutADrop(t *testing.T) {
	cases := map[string]*syscall.Credential{
		"no credential": nil,
		"still root":    {Uid: 0, Gid: 0},
	}

	for name, credential := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := inheritedCapabilities(credential)
			if err != nil {
				t.Fatalf("inheritedCapabilities: %v", err)
			}
			if got != nil {
				t.Errorf("got the ambient set %v, want none", got)
			}
		})
	}
}

// A VM's shard-init has no PATH of its own, so argv[0] resolves on the entrypoint's PATH and never on ours.
func TestLookPathUsesTheEntrypointsPath(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a fixture the test executes
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent")

	got, err := lookPath(entrypoint{argv: []string{"tool"}, env: []string{"HOME=/", "PATH=/nonexistent:" + dir}})
	if err != nil || got != tool {
		t.Fatalf("lookPath = %q, %v; want %q", got, err, tool)
	}
	if _, err := lookPath(entrypoint{argv: []string{"tool"}, env: []string{"PATH=/nonexistent"}}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("a missing tool resolved: %v", err)
	}
	if got, err := lookPath(entrypoint{argv: []string{tool}}); err != nil || got != tool {
		t.Fatalf("an absolute argv[0] = %q, %v", got, err)
	}
}

// ForkExec changes into the workdir before the exec, so a relative argv[0] and a relative PATH entry mean the workdir.
func TestLookPathResolvesRelativeNamesAgainstTheWorkDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "server"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a fixture the test executes
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	if got, err := lookPath(entrypoint{argv: []string{"./bin/server"}, dir: dir}); err != nil || got != "./bin/server" {
		t.Fatalf("a relative argv[0] = %q, %v", got, err)
	}
	if got, err := lookPath(entrypoint{argv: []string{"server"}, env: []string{"PATH=/nonexistent:bin"}, dir: dir}); err != nil || got != "bin/server" {
		t.Fatalf("a relative PATH entry = %q, %v", got, err)
	}
	if _, err := lookPath(entrypoint{argv: []string{"./bin/server"}, dir: t.TempDir()}); err == nil {
		t.Fatal("a name outside the workdir resolved")
	}
}

// fd 0 is the host's exit file, opened for append; on sysbox guest root can append to it too (SHARD-365).
func TestFileReporterKeepsOneExitRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	stdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = stdin })

	if _, err := f.WriteString(strings.Repeat("guest bytes with no newline ", 1<<15)); err != nil {
		t.Fatal(err)
	}
	for code := range 300 {
		if err := (&fileReporter{}).exited(models.ExitStatus{Code: code % 256}); err != nil {
			t.Fatal(err)
		}
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(models.ExitReport{Kind: models.ExitReportKind, Code: 299 % 256})
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "\n"+string(want)+"\n" {
		t.Fatalf("the exit file holds %d bytes, want only the last record %s", len(blob), want)
	}
}
