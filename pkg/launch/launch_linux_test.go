//go:build linux

package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The test binary plays both processes the runtime would run: the middle stands in for runc, the shim for shard-init.
const (
	middleRole = "launch-test-middle"
	shimRole   = "launch-test-shim"
	noShimRole = "launch-test-no-shim"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == shimRole {
		runtime.LockOSThread()
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == middleRole {
		os.Exit(runMiddle(os.Args[2], os.Args[3:]))
	}
	if len(os.Args) > 1 && os.Args[1] == shimRole {
		os.Exit(runShim(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == noShimRole {
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// runShim is shard-init's launch mode, left dumpable so a test that is not root can trace it.
func runShim(argv []string) int {
	err := shim(argv, false)
	var failed *NotStartedError
	if !errors.As(err, &failed) {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}
	if failed.NotFound() {
		return 127
	}

	return 126
}

// runMiddle is runc: it keeps its own copy of the channel, writes the shim's pid once it started, and reaps it.
func runMiddle(pidFile string, argv []string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}

	cmd := exec.Command(self, append([]string{shimRole}, argv...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{os.NewFile(fd, "launch")}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}

	tmp := pidFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}
	if err := os.Rename(tmp, pidFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}

	var exit *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}

	return cmd.ProcessState.ExitCode()
}

// launchRun is one launch through the stand-in runtime.
type launchRun struct {
	ch      *Channel
	cmd     *exec.Cmd
	pidFile string
	waited  bool
}

func start(t *testing.T, role string, argv []string, setup ...func(*exec.Cmd)) *launchRun {
	t.Helper()

	ch, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}

	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command(self, append([]string{role, pidFile}, argv...)...)
	cmd.ExtraFiles = []*os.File{ch.Guest()}
	for _, f := range setup {
		f(cmd)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the stand-in runtime: %v", err)
	}
	if err := ch.CloseGuest(); err != nil {
		t.Fatalf("CloseGuest: %v", err)
	}

	r := &launchRun{ch: ch, cmd: cmd, pidFile: pidFile}
	t.Cleanup(func() {
		if err := ch.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if r.waited {
			return
		}
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the stand-in runtime: %v", err)
		}
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Errorf("reap the stand-in runtime: %v", err)
		}
	})

	return r
}

func (r *launchRun) pid() (int, error) {
	blob, err := os.ReadFile(r.pidFile)
	if err != nil {
		return 0, err
	}

	return strconv.Atoi(strings.TrimSpace(string(blob)))
}

// shimPID is the pid the runtime wrote, which the test reads once the launch is over.
func (r *launchRun) shimPID(t *testing.T) int {
	t.Helper()

	pid, err := r.pid()
	if err != nil {
		t.Fatalf("read the pid file: %v", err)
	}

	return pid
}

func (r *launchRun) await(t *testing.T, pidOf func() (int, error)) (int, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	return r.ch.Await(ctx, pidOf)
}

// exit is the runtime's exit code, which carries the shim's or the command's end.
func (r *launchRun) exit(t *testing.T) int {
	t.Helper()

	r.waited = true
	err := r.cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("wait for the stand-in runtime: %v", err)
	}

	return r.cmd.ProcessState.ExitCode()
}

func TestALaunchIsTheExecEvent(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})

	pid, err := r.await(t, r.pid)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if want := r.shimPID(t); pid != want {
		t.Errorf("Await returned pid %d, want %d from the pid file", pid, want)
	}
	if code := r.exit(t); code != 0 {
		t.Errorf("the command exited %d, want 0", code)
	}
}

// A command that ends or dies right after its exec still launched, and its own end reaches the runtime.
func TestACommandThatEndsAtOnceStillLaunched(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   int
	}{
		{"exit 3", 3},
		{"kill -9 $$", 128 + int(unix.SIGKILL)},
	} {
		t.Run(tc.script, func(t *testing.T) {
			r := start(t, middleRole, []string{"/bin/sh", "-c", tc.script})

			if _, err := r.await(t, r.pid); err != nil {
				t.Fatalf("Await: %v", err)
			}
			if code := r.exit(t); code != tc.want {
				t.Errorf("the command exited %d, want %d", code, tc.want)
			}
		})
	}
}

func TestACommandThatCannotStartIsRefusedWithItsErrno(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "plain"), "#!/bin/sh\n", 0o644)
	write(t, filepath.Join(dir, "orphan"), "#!/no/such/interpreter\n", 0o755)
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for _, tc := range []struct {
		name string
		argv []string
		want syscall.Errno
	}{
		{"a missing path", []string{"/no/such/binary"}, unix.ENOENT},
		{"a name on no PATH entry", []string{"nosuchcommand"}, unix.ENOENT},
		{"a file with no execute bit", []string{filepath.Join(dir, "plain")}, unix.EACCES},
		{"a name on PATH with no execute bit", []string{"plain"}, unix.EACCES},
		{"a missing interpreter", []string{filepath.Join(dir, "orphan")}, unix.ENOENT},
		{"a directory", []string{filepath.Join(dir, "folder")}, unix.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := start(t, middleRole, tc.argv, func(cmd *exec.Cmd) { cmd.Env = []string{"PATH=" + dir} })

			_, err := r.await(t, r.pid)
			var failed *NotStartedError
			if !errors.As(err, &failed) {
				t.Fatalf("Await returned %v, want a NotStartedError", err)
			}
			if failed.Errno != tc.want {
				t.Errorf("the errno is %v, want %v", failed.Errno, tc.want)
			}
			if failed.Reason() != tc.want.Error() {
				t.Errorf("the reason is %q, want the kernel's %q", failed.Reason(), tc.want.Error())
			}

			want := 126
			if failed.NotFound() {
				want = 127
			}
			if code := r.exit(t); code != want {
				t.Errorf("the shim exited %d, want %d", code, want)
			}
		})
	}
}

// With no PATH in the env the search uses the default one, as execvp and runc do.
func TestANameWithNoPATHSearchesTheDefault(t *testing.T) {
	r := start(t, middleRole, []string{"true"}, func(cmd *exec.Cmd) { cmd.Env = []string{} })

	if _, err := r.await(t, r.pid); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if code := r.exit(t); code != 0 {
		t.Errorf("the command exited %d, want 0", code)
	}
}

func TestTheCommandKeepsItsArgvEnvAndCwd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	r := start(t, middleRole, []string{"/bin/sh", "-c", `{ pwd; printf '%s\n' "$@"; env | sort; } > ` + out, "sh", "a", "b c"},
		func(cmd *exec.Cmd) {
			cmd.Dir = dir
			cmd.Env = []string{"FOO=bar baz", "PATH=/usr/bin:/bin"}
		})

	if _, err := r.await(t, r.pid); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if code := r.exit(t); code != 0 {
		t.Fatalf("the command exited %d, want 0", code)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read what the command wrote: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	want := []string{dir, "a", "b c", "FOO=bar baz", "PATH=/usr/bin:/bin"}
	// sh exports PWD, SHLVL and _ on its own, so only the lines the caller set are compared.
	kept := slices.DeleteFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "PWD=") || strings.HasPrefix(l, "OLDPWD=") || strings.HasPrefix(l, "SHLVL=") || strings.HasPrefix(l, "_=")
	})
	if strings.Join(kept, "\n") != strings.Join(want, "\n") {
		t.Errorf("the command saw %q, want %q", kept, want)
	}
}

func TestAShimThatDiesBeforeTheTraceDidNotStart(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})

	_, err := r.await(t, func() (int, error) {
		pid, err := r.pid()
		if err != nil {
			return 0, err
		}

		return pid, unix.Kill(pid, unix.SIGKILL)
	})

	var failed *NotStartedError
	if !errors.As(err, &failed) || failed.Errno != 0 {
		t.Fatalf("Await returned %v, want a NotStartedError with no errno", err)
	}
	if code := r.exit(t); code != 128+int(unix.SIGKILL) {
		t.Errorf("the shim exited %d, want the kill", code)
	}
}

// The shim stops before the trace, so every signal below lands inside the window between the attach and the exec.
func stopped(r *launchRun) func() (int, error) {
	return func() (int, error) {
		pid, err := r.pid()
		if err != nil {
			return 0, err
		}

		return pid, unix.Kill(pid, unix.SIGSTOP)
	}
}

func TestAShimKilledInsideTheTraceDidNotStart(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})
	sent := killWhenTraced(r, unix.SIGKILL)

	_, err := r.await(t, stopped(r))
	if err := <-sent; err != nil {
		t.Fatal(err)
	}

	var failed *NotStartedError
	if !errors.As(err, &failed) || failed.Errno != 0 {
		t.Fatalf("Await returned %v, want a NotStartedError with no errno", err)
	}
	if code := r.exit(t); code != 128+int(unix.SIGKILL) {
		t.Errorf("the shim exited %d, want the kill", code)
	}
}

// The trace passes a signal on, so a shim that a SIGTERM ends before its exec dies of it and did not start.
func TestASignalInsideTheTraceReachesTheShim(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})
	sent := killWhenTraced(r, unix.SIGTERM, unix.SIGCONT)

	_, err := r.await(t, stopped(r))
	if err := <-sent; err != nil {
		t.Fatal(err)
	}

	var failed *NotStartedError
	if !errors.As(err, &failed) || failed.Errno != 0 {
		t.Fatalf("Await returned %v, want a NotStartedError with no errno", err)
	}
	if code := r.exit(t); code != 128+int(unix.SIGTERM) {
		t.Errorf("the shim exited %d, want the SIGTERM", code)
	}
}

// A stopped shim stays stopped under the trace, as its real parent sees it, and a SIGCONT lets the launch finish.
func TestAStopInsideTheTraceStaysAStop(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})

	held := make(chan error, 1)
	go func() {
		pid, err := tracedPID(r)
		if err != nil {
			held <- err
			return
		}

		time.Sleep(100 * time.Millisecond)
		state, err := procState(pid)
		if err != nil {
			held <- err
			return
		}
		if state != "t" && state != "T" {
			held <- fmt.Errorf("the traced shim is in state %q, want a stop", state)
			return
		}

		held <- unix.Kill(pid, unix.SIGCONT)
	}()

	if _, err := r.await(t, stopped(r)); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if code := r.exit(t); code != 0 {
		t.Errorf("the command exited %d, want 0", code)
	}
}

func TestLaunchesSideBySideEachGetTheirOwnPID(t *testing.T) {
	const n = 8

	runs := make([]*launchRun, n)
	for i := range runs {
		runs[i] = start(t, middleRole, []string{"/bin/true"})
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, r := range runs {
		wg.Go(func() {
			pid, err := r.await(t, r.pid)
			if err != nil {
				errs[i] = err
				return
			}
			want, err := r.pid()
			if err != nil {
				errs[i] = err
				return
			}
			if pid != want {
				errs[i] = fmt.Errorf("launch %d returned pid %d, want %d", i, pid, want)
			}
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("launch %d: %v", i, err)
		}
	}
}

// A pid that names another launch's shim, or any process without this channel, is not this launch, and the other one is left alone.
func TestAPIDOfAnotherLaunchIsNotThisLaunch(t *testing.T) {
	other := start(t, middleRole, []string{"/bin/true"})
	otherPID := readyShim(t, other)

	r := start(t, middleRole, []string{"/bin/true"})
	if _, err := r.await(t, func() (int, error) { return otherPID, nil }); !isNotStarted(err) {
		t.Fatalf("Await on another launch's pid returned %v, want a NotStartedError", err)
	}
	// The host hangs up on a shim it never let go, so the runtime waiting on it ends too.
	if code := r.exit(t); code != 125 {
		t.Errorf("the shim the host never let go exited %d, want 125", code)
	}

	if _, err := other.await(t, other.pid); err != nil {
		t.Fatalf("the other launch: %v", err)
	}
	if code := other.exit(t); code != 0 {
		t.Errorf("the other command exited %d, want 0", code)
	}
}

func TestARuntimeThatEndsBeforeTheShimIsNoShim(t *testing.T) {
	r := start(t, noShimRole, nil)

	if _, err := r.await(t, r.pid); !errors.Is(err, ErrNoShim) {
		t.Fatalf("Await returned %v, want ErrNoShim", err)
	}
}

func TestACancelledWaitEndsBeforeTheShim(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.ch.Await(ctx, r.pid); !errors.Is(err, context.Canceled) {
		t.Fatalf("Await returned %v, want the cancellation", err)
	}
	if code := r.exit(t); code != 125 {
		t.Errorf("the shim exited %d, want 125 for the host's hang-up", code)
	}
}

// A cancel while the trace holds a stopped shim kills it, so no stopped shim is left with nobody to answer it.
func TestACancelInsideTheTraceKillsTheShim(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/true"})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	held := make(chan error, 1)
	go func() {
		_, err := tracedPID(r)
		cancel()
		held <- err
	}()

	_, err := r.ch.Await(ctx, stopped(r))
	if herr := <-held; herr != nil {
		t.Fatal(herr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Await returned %v, want the cancellation", err)
	}
	if code := r.exit(t); code != 128+int(unix.SIGKILL) {
		t.Errorf("the shim exited %d, want the kill", code)
	}
}

// The pin outlives the launch, so a cancel after the exec still ends the command and nothing that reused its pid.
func TestAKillAfterTheLaunchEndsTheCommand(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/sleep", "30"})

	if _, err := r.await(t, r.pid); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if err := r.ch.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if code := r.exit(t); code != 128+int(unix.SIGKILL) {
		t.Errorf("the command exited %d, want the kill", code)
	}
}

// A Kill before the trace pins the shim is kept, so the shim dies the moment its identity is proven and never runs the command.
func TestAKillBeforeThePinEndsTheShimUnstarted(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/sleep", "30"})

	if err := r.ch.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := r.await(t, r.pid); !isNotStarted(err) {
		t.Fatalf("Await returned %v, want a command that never started", err)
	}
	if code := r.exit(t); code != 128+int(unix.SIGKILL) {
		t.Errorf("the shim exited %d, want the kill", code)
	}
}

func TestARecordIsOneErrno(t *testing.T) {
	for _, tc := range []struct {
		blob string
		want syscall.Errno
	}{
		{string(record(unix.ENOENT)), unix.ENOENT},
		{"E13", unix.EACCES},
		{"", 0},
		{"E", 0},
		{"E0", 0},
		{"E4096", 0},
		{"Eabc", 0},
		{"R2", 0},
	} {
		if got := parseRecord([]byte(tc.blob)); got != tc.want {
			t.Errorf("parseRecord(%q) = %v, want %v", tc.blob, got, tc.want)
		}
	}
}

// killWhenTraced sends sigs once the host's trace holds the shim, and says how that went.
func killWhenTraced(r *launchRun, sigs ...syscall.Signal) <-chan error {
	sent := make(chan error, 1)
	go func() {
		pid, err := tracedPID(r)
		if err != nil {
			sent <- err
			return
		}

		var errs []error
		for _, sig := range sigs {
			if err := unix.Kill(pid, sig); err != nil {
				errs = append(errs, fmt.Errorf("send %v to the shim: %w", sig, err))
			}
		}
		sent <- errors.Join(errs...)
	}()

	return sent
}

// tracedPID waits until the shim has a tracer.
func tracedPID(r *launchRun) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid, err := r.pid()
		if err == nil && tracer(pid) != 0 {
			return pid, nil
		}
		time.Sleep(time.Millisecond)
	}

	return 0, errors.New("the shim never had a tracer")
}

// readyShim waits until the shim holds its end of the channel at fd 3, which it does once it is ready.
func readyShim(t *testing.T, r *launchRun) int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pid, err := r.pid()
		if err == nil && r.ch.holds(pid) == nil {
			return pid
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the shim never held its channel")

	return 0
}

func tracer(pid int) int {
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "TracerPid:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return 0
			}
			return n
		}
	}

	return 0
}

func procState(pid int) (string, error) {
	fields, err := procStat(pid)
	if err != nil {
		return "", err
	}

	return fields[0], nil
}

func isNotStarted(err error) bool {
	var failed *NotStartedError
	return errors.As(err, &failed)
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
