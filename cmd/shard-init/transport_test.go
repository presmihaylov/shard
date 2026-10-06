package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

func shortDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "si") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("make the socket dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})

	return dir
}

// startTransport runs this binary as shard-init -transport unix:<dir>, the vsock mode over unix sockets.
func startTransport(t *testing.T) (*exec.Cmd, supervisor.Dialer) {
	t.Helper()

	return startTransportUnder(t, 0)
}

// startTransportUnder is the same with a descriptor limit on the supervisor; zero keeps the test's own.
func startTransportUnder(t *testing.T, limit uint64) (*exec.Cmd, supervisor.Dialer) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	dir := shortDir(t)
	cmd := exec.Command(exe, "-transport", "unix:"+dir)
	if limit > 0 {
		// The hard limit too, or the Go runtime raises the soft one back at startup; the shell keeps the pid.
		cmd = exec.Command("/bin/sh", "-c", `ulimit -n "$1" && exec "$2" -transport "$3"`, "sh", strconv.FormatUint(limit, 10), exe, "unix:"+dir)
	}
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}
		_ = cmd.Wait()
	})

	dial := func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	}

	return cmd, dial
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func childArgv(role string) []string {
	exe, _ := os.Executable()

	return []string{exe, childPrefix + role}
}

func awaitKind(t *testing.T, c *supervisor.Control, kind string) supervisor.Message {
	t.Helper()
	for {
		m, err := c.Next()
		if err != nil {
			t.Fatalf("read the control channel: %v", err)
		}
		if m.Kind == kind {
			return m
		}
	}
}

func TestTransportRunReportsReadyThenExit(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)

	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("exit:7"), Env: os.Environ()}); err != nil {
		t.Fatalf("run: %v", err)
	}
	exit := awaitKind(t, c, supervisor.KindExit)
	if exit.Exit == nil || exit.Exit.Code != 7 {
		t.Fatalf("exit = %+v, want code 7", exit.Exit)
	}

	// A second control connection, the way a restarted daemon comes back, hears the state first.
	again, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect again: %v", err)
	}
	defer again.Close()
	state, err := again.Next()
	if err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if state.Kind != supervisor.KindState || !state.Ready || state.Exit == nil || state.Exit.Code != 7 || state.Logs != supervisor.LogsVersion {
		t.Fatalf("state = %+v, want ready with exit code 7 and logs version %d", state, supervisor.LogsVersion)
	}
	if err := again.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("exit:0")}); !errors.Is(err, supervisor.ErrEntrypointNotStarted) {
		t.Fatalf("a second run gave %v, want ErrEntrypointNotStarted", err)
	}
}

// An empty run is a sandbox created with no command: the guest says ready, forks nothing, and a stop ends it with no exit.
func TestTransportAnEmptyRunIsReadyAndForksNothing(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)

	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Env: os.Environ()}); err != nil {
		t.Fatalf("an empty run: %v", err)
	}
	awaitKind(t, c, supervisor.KindReady)

	again, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect again: %v", err)
	}
	defer again.Close()
	state, err := again.Next()
	if err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if state.Kind != supervisor.KindState || !state.Ready || state.Exit != nil {
		t.Fatalf("state = %+v, want ready with no exit", state)
	}
	if err := again.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("exit:0")}); !errors.Is(err, supervisor.ErrEntrypointNotStarted) {
		t.Fatalf("a second run gave %v, want ErrEntrypointNotStarted", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the supervisor ended with %v, want a clean exit", err)
		}
	case <-ctx.Done():
		t.Fatal("the supervisor did not end at once, so the empty run left a child to wait for")
	}
	for {
		m, err := again.Next()
		if err != nil {
			break
		}
		if m.Kind == supervisor.KindExit {
			t.Fatalf("the guest reported the exit %+v, but nothing ran", m.Exit)
		}
	}
}

func TestTransportRunFailureNamesTheBinary(t *testing.T) {
	_, dial := startTransport(t)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	err = c.Run(t.Context(), supervisor.RunSpec{Argv: []string{"/nonexistent/entrypoint"}})
	if !errors.Is(err, supervisor.ErrEntrypointNotStarted) || !strings.Contains(err.Error(), "/nonexistent/entrypoint") {
		t.Fatalf("run gave %v, want ErrEntrypointNotStarted naming the binary", err)
	}
}

func TestTransportExecMovesTheStreams(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = stdinW.WriteString("hello over vsock")
		_ = stdinW.Close()
	}()
	var pid int
	spec := models.ExecSpec{Stdin: stdinR, Stdout: stdoutW, Stderr: stderrW, Report: func(p int) { pid = p }}
	exit, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("echo:3"), Env: os.Environ()}, spec)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()
	if exit.Code != 3 {
		t.Fatalf("exit = %+v, want code 3", exit)
	}
	if pid <= 0 {
		t.Fatalf("pid = %d, want the started frame", pid)
	}
	var stdout, stderr bytes.Buffer
	_, _ = stdout.ReadFrom(stdoutR)
	_, _ = stderr.ReadFrom(stderrR)
	if stdout.String() != "hello over vsock" || stderr.String() != "echo-err" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

// A path that is not there and a name no PATH entry holds are both 127, so shell can tell a missing sh on every substrate (SHARD-759).
func TestTransportExecNotStartedReports127(t *testing.T) {
	for name, header := range map[string]supervisor.ExecHeader{
		"a path": {Argv: []string{"/nonexistent/cmd"}},
		"a name": {Argv: []string{"sh"}, Env: []string{"PATH=/nonexistent"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, dial := startTransport(t)
			ctx := testContext(t)
			c, err := supervisor.Connect(ctx, dial)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
				t.Fatalf("run: %v", err)
			}

			_, err = supervisor.Exec(ctx, dial, "sb", header, models.ExecSpec{})
			var notStarted *models.CommandNotStartedError
			if !errors.As(err, &notStarted) || notStarted.Code != 127 {
				t.Fatalf("exec gave %v, want CommandNotStartedError with code 127", err)
			}
		})
	}
}

// SHARD-764: an exec on runc before 1.2 inherits the daemon's umask, 0077 under setup's unit.
func TestTransportExecStartsAtUmask0022WhateverTheHostGave(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(ctx, supervisor.RunSpec{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	exit, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"/bin/sh", "-c", "umask"}}, models.ExecSpec{Stdout: stdoutW})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	_ = stdoutW.Close()
	var stdout bytes.Buffer
	_, _ = stdout.ReadFrom(stdoutR)
	if exit.Code != 0 || stdout.String() != "0022\n" {
		t.Fatalf("exit %+v, umask %q, want 0 and 0022", exit, stdout.String())
	}
}

// docker run -w makes a missing work directory, with no command as with one, since every exec defaults to it (SHARD-757).
func TestTransportARunMakesItsWorkDirectory(t *testing.T) {
	for name, argv := range map[string][]string{"no command": nil, "a command": childArgv("sleep:60000")} {
		t.Run(name, func(t *testing.T) {
			_, dial := startTransport(t)
			c, err := supervisor.Connect(testContext(t), dial)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			dir := filepath.Join(shortDir(t), "work", "deep")
			if err := c.Run(t.Context(), supervisor.RunSpec{Argv: argv, Env: os.Environ(), WorkDir: dir}); err != nil {
				t.Fatalf("run: %v", err)
			}
			awaitKind(t, c, supervisor.KindReady)

			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("the work directory after the run: %v, want a directory", err)
			}
		})
	}
}

func TestTransportARunRefusesAWorkDirectoryItCannotMake(t *testing.T) {
	_, dial := startTransport(t)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	file := filepath.Join(shortDir(t), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err = c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000"), WorkDir: filepath.Join(file, "work")})
	if !errors.Is(err, supervisor.ErrEntrypointNotStarted) || !strings.Contains(err.Error(), file) {
		t.Fatalf("run gave %v, want ErrEntrypointNotStarted naming the work directory", err)
	}
}

// docker restart makes the work directory again, so a start again after the app removed it still runs.
func TestTransportARestartMakesTheWorkDirectoryAgain(t *testing.T) {
	_, dial := startTransport(t)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	dir := filepath.Join(shortDir(t), "work")
	spec := supervisor.RunSpec{Argv: childArgv("run:300:1"), Env: os.Environ(), WorkDir: dir, Restart: models.RestartOnFailure, Retries: 1, Backoff: time.Millisecond}
	if err := c.Run(t.Context(), spec); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove the work directory: %v", err)
	}

	restarts := awaitKind(t, c, supervisor.KindRestarts)
	if restarts.Restarts == nil || restarts.Restarts.Count != 1 || restarts.Restarts.GaveUp {
		t.Fatalf("restarts = %+v, want one start again", restarts.Restarts)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the work directory after the restart: %v, want a directory", err)
	}
}

// docker exec -w never makes the directory and answers 126; fork's chdir would name the binary instead (SHARD-757).
func TestTransportAnExecRefusesAWorkDirectoryAndNamesIt(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	root := shortDir(t)
	work := filepath.Join(root, "work")
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000"), WorkDir: work}); err != nil {
		t.Fatalf("run: %v", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The sandbox's own work directory, removed after the start, is the one every exec defaults to.
	if err := os.Remove(work); err != nil {
		t.Fatalf("remove the work directory: %v", err)
	}

	for dir, want := range map[string]string{
		filepath.Join(root, "missing"): "does not exist",
		work:                           "does not exist",
		file:                           "is not a directory",
	} {
		argv := childArgv("exit:0")
		_, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: argv, Env: os.Environ(), WorkDir: dir}, models.ExecSpec{})
		var notStarted *models.CommandNotStartedError
		if !errors.As(err, &notStarted) || notStarted.Code != 126 {
			t.Fatalf("an exec in %s gave %v, want CommandNotStartedError with code 126", dir, err)
		}
		if !strings.Contains(notStarted.Reason, strconv.Quote(dir)+" "+want) || strings.Contains(notStarted.Reason, argv[0]) {
			t.Fatalf("the reason %q names the binary or misses %q %s", notStarted.Reason, dir, want)
		}
	}
	for _, dir := range []string{filepath.Join(root, "missing"), work} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the exec made %s: %v", dir, err)
		}
	}
}

// A PID 1 starts under the kernel's umask of 022, which leaves docker's 0755 as it is.
func TestMakeWorkDirMakesEveryLevelAsDockerDoes(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b")

	if err := makeWorkDir(dir); err != nil {
		t.Fatalf("make the work directory: %v", err)
	}
	for _, level := range []string{filepath.Join(root, "a"), dir} {
		info, err := os.Stat(level)
		if err != nil {
			t.Fatalf("stat %s: %v", level, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o755 {
			t.Fatalf("%s is %v, want a 0755 directory", level, info.Mode())
		}
	}
	if err := makeWorkDir(dir); err != nil {
		t.Fatalf("make the work directory again: %v", err)
	}
}

func TestTransportSignalRefusesAForeignPID(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	err = c.Signal(t.Context(), os.Getpid(), "KILL")
	if err == nil || !strings.Contains(err.Error(), "not a process shard-init started") {
		t.Fatalf("signal gave %v, want the refusal", err)
	}
}

// With no command the entrypoint pid is 0, so pid 0 and a negative pid must never reach kill(2) as a group.
func TestTransportSignalRefusesAProcessGroup(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{}); err != nil {
		t.Fatalf("run with no command: %v", err)
	}

	for _, pid := range []int{0, -1} {
		err = c.Signal(t.Context(), pid, "KILL")
		if err == nil || !strings.Contains(err.Error(), "names a process group") {
			t.Fatalf("signal to pid %d gave %v, want the refusal", pid, err)
		}
	}
}

// A short seed would rekey the crng from little more than the state every fork of the save shares (SHARD-293).
func TestTransportRefusesAShortReseed(t *testing.T) {
	for _, seed := range [][]byte{nil, make([]byte, supervisor.SeedSize-1)} {
		err := (&transport{}).handle(supervisor.Message{Kind: supervisor.KindReseed, Seed: seed})
		if err == nil || !strings.Contains(err.Error(), "under the 32 a crng key takes") {
			t.Fatalf("a reseed of %d bytes gave %v, want the refusal", len(seed), err)
		}
	}
}

// A guest on tsc wakes from a restore at the time of its save, so the reseed sets its clock to the host's (SHARD-776).
func TestTransportReseedSetsTheClockToTheHostTime(t *testing.T) {
	var set int64
	tr := &transport{setClock: func(ns int64) error {
		set = ns

		return nil
	}}
	now := time.Now().UnixNano()
	if err := tr.handle(supervisor.Message{Kind: supervisor.KindReseed, Seed: make([]byte, supervisor.SeedSize), Now: now}); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if set != now {
		t.Fatalf("the reseed set the clock to %d, want the host's %d", set, now)
	}
}

func TestTransportRefusesAReseedWithNoHostTime(t *testing.T) {
	err := (&transport{}).handle(supervisor.Message{Kind: supervisor.KindReseed, Seed: make([]byte, supervisor.SeedSize)})
	if err == nil || !strings.Contains(err.Error(), "carries no host time") {
		t.Fatalf("a reseed with no time gave %v, want the refusal", err)
	}
}

// The guest refuses a reseed with no time, so one that lands proves the host sent its own.
func TestTransportTakesTheHostsReseed(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	if err := c.Reseed(ctx); err != nil {
		t.Fatalf("reseed: %v", err)
	}
}

func TestTransportExecCancelKillsTheCommand(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	execCtx, cancel := context.WithCancel(ctx)
	started := make(chan int, 1)
	spec := models.ExecSpec{Report: func(p int) { started <- p }}
	done := make(chan error, 1)
	go func() {
		_, err := supervisor.Exec(execCtx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("sleep:60000")}, spec)
		done <- err
	}()
	pid := <-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("exec gave %v, want context.Canceled", err)
	}
	// The supervisor reaps its child, so a signal of zero says ESRCH once the command is gone.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still runs after the cancel", pid)
}

// A hang-up with no cancel frame is a daemon restart: the command runs on and its output drains, as on gVisor (SHARD-270).
func TestTransportExecOutlivesAHangUp(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	conn, err := dial(ctx, supervisor.ExecPort)
	if err != nil {
		t.Fatalf("open an exec connection: %v", err)
	}
	marker := filepath.Join(shortDir(t), "drained")
	if err := supervisor.WriteMessage(conn, supervisor.ExecHeader{Argv: childArgv("spew:" + marker)}); err != nil {
		t.Fatalf("write the header: %v", err)
	}
	stream, payload, err := supervisor.ReadFrame(conn)
	if err != nil || stream != supervisor.StreamStarted {
		t.Fatalf("first frame = %d %v, want the started frame", stream, err)
	}
	var started supervisor.StartedFrame
	if err := supervisor.DecodeFrame(payload, &started); err != nil {
		t.Fatalf("decode the started frame: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(started.PID, syscall.SIGKILL); err != nil {
			t.Errorf("kill pid %d: %v", started.PID, err)
		}
	})
	if err := conn.Close(); err != nil {
		t.Fatalf("hang up: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the command never got past its output after the hang-up: %v", err)
	}
	if err := syscall.Kill(started.PID, 0); err != nil {
		t.Fatalf("pid %d is gone after a hang-up: %v", started.PID, err)
	}
}

// syncBuffer is a bytes.Buffer the logs goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// Resume takes all the guest holds, as a log that never saw this guest does.
func (b *syncBuffer) Resume(from, _ uint64) (uint64, error) { return from, nil }

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestTransportLogsFollowTheEntrypoint(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	// The entrypoint speaks before any logs connection exists, so the bytes must wait for one.
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("say:first line")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitKind(t, c, supervisor.KindExit)

	logsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- supervisor.Logs(logsCtx, dial, &logs, supervisor.LogsVersion) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "first line") {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "first line\n") {
		t.Fatalf("logs = %q, want the entrypoint's line", logs.String())
	}
}

// A fake pump takes the app's last output and acks it late, the way a host still writing its log file would; the end waits for that ack.
func TestTransportEndWaitsForTheHostToLandTheLastOutput(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	pump, held := openLogs(ctx, t, dial)
	defer pump.Close()
	if err := binary.Write(pump, binary.BigEndian, held[0]); err != nil {
		t.Fatalf("resume the logs: %v", err)
	}

	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("say:last words")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	tail := make([]byte, len("last words\n"))
	if _, err := io.ReadFull(pump, tail); err != nil || string(tail) != "last words\n" {
		t.Fatalf("the pump read %q and %v, want the app's last line", tail, err)
	}
	select {
	case <-ended(c):
		t.Fatal("the guest reported the end before the host acked the app's last output")
	case <-time.After(500 * time.Millisecond):
	}

	// A host that attaches meanwhile hears the policy still going, so its run waits for the same ack.
	again, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect again: %v", err)
	}
	defer again.Close()
	state, err := again.Next()
	if err != nil {
		t.Fatalf("read the state: %v", err)
	}
	if state.Exit == nil || state.Restarts == nil || state.Restarts.Ended {
		t.Fatalf("state = %+v with restarts %+v, want the exit and the policy not yet ended", state, state.Restarts)
	}

	if err := binary.Write(pump, binary.BigEndian, held[0]+uint64(len(tail))); err != nil {
		t.Fatalf("ack the last output: %v", err)
	}
	if restarts := awaitKind(t, again, supervisor.KindRestarts); restarts.Restarts == nil || !restarts.Restarts.Ended {
		t.Fatalf("restarts = %+v, want the app ended once its output landed", restarts.Restarts)
	}
}

// A host that comes back already holding the last output has no write to ack, so its resume is what lets the end go.
func TestTransportEndFollowsAHostThatResumesPastTheLastOutput(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	first, held := openLogs(ctx, t, dial)
	if err := binary.Write(first, binary.BigEndian, held[0]); err != nil {
		t.Fatalf("resume the logs: %v", err)
	}
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("say:last words")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	tail := make([]byte, len("last words\n"))
	if _, err := io.ReadFull(first, tail); err != nil {
		t.Fatalf("read the app's last line: %v", err)
	}
	end := ended(c)
	// The host lands the line and dies before its ack.
	if err := first.Close(); err != nil {
		t.Fatalf("close the first logs connection: %v", err)
	}

	second, held := openLogs(ctx, t, dial)
	defer second.Close()
	if held[1]-held[0] != uint64(len(tail)) {
		t.Fatalf("the guest holds %v, want the %d bytes of the unacked line", held, len(tail))
	}
	if err := binary.Write(second, binary.BigEndian, held[1]); err != nil {
		t.Fatalf("resume the logs past the line: %v", err)
	}
	select {
	case <-end:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest never reported the end to a host that resumed past the last output")
	}
}

// A host whose log refuses the output acks nothing more and says so, so the end goes without that ack and the guest hangs up.
func TestTransportEndFollowsAHostWhoseLogStopped(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	pump, held := openLogs(ctx, t, dial)
	defer pump.Close()
	if err := binary.Write(pump, binary.BigEndian, held[0]); err != nil {
		t.Fatalf("resume the logs: %v", err)
	}
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("say:last words")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	tail := make([]byte, len("last words\n"))
	if _, err := io.ReadFull(pump, tail); err != nil {
		t.Fatalf("read the app's last line: %v", err)
	}
	end := ended(c)
	if err := binary.Write(pump, binary.BigEndian, supervisor.LogsStopped); err != nil {
		t.Fatalf("say the log stopped: %v", err)
	}
	select {
	case <-end:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest never reported the end after the host said its log stopped")
	}
	if err := pump.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("bound the read: %v", err)
	}
	if _, err := io.Copy(io.Discard, pump); err != nil {
		t.Fatalf("the guest kept the logs connection open after the stop: %v", err)
	}
}

// openLogs dials the logs port as a host does and reads the output bytes [from, to) the guest holds.
func openLogs(ctx context.Context, t *testing.T, dial supervisor.Dialer) (net.Conn, [2]uint64) {
	t.Helper()

	conn, err := dial(ctx, supervisor.LogsPort)
	if err != nil {
		t.Fatalf("open the logs connection: %v", err)
	}
	var held [2]uint64
	if err := binary.Read(conn, binary.BigEndian, &held); err != nil {
		t.Fatalf("read the output the guest holds: %v", err)
	}

	return conn, held
}

// ended closes once c hears the policy end.
func ended(c *supervisor.Control) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for {
			m, err := c.Next()
			if err != nil {
				return
			}
			if m.Kind == supervisor.KindRestarts && m.Restarts != nil && m.Restarts.Ended {
				close(done)

				return
			}
		}
	}()

	return done
}

func TestTransportStopEndsTheSupervisor(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitKind(t, c, supervisor.KindExit)

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the supervisor ended with %v, want a clean exit", err)
		}
	case <-ctx.Done():
		t.Fatal("the supervisor did not exit after the stop")
	}
}

func TestTransportKillForcesTheEntrypointDown(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// KindKill is the forced stop: it SIGKILLs the entrypoint, freezes the rest, then flushes before the host cuts the VM.
	if err := c.Kill(t.Context()); err != nil {
		t.Fatalf("kill: %v", err)
	}
	exit := awaitKind(t, c, supervisor.KindExit)
	if exit.Exit == nil || exit.Exit.Signal != int(syscall.SIGKILL) {
		t.Fatalf("exit = %+v, want signal SIGKILL", exit.Exit)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the supervisor ended with %v, want a clean exit", err)
		}
	case <-ctx.Done():
		t.Fatal("the supervisor did not exit after the kill")
	}
}

// A VM's run stops its app over the control channel: the stop terms it, ends the policy, and the guest stays up.
func TestTransportStopAppEndsTheAppAndKeepsTheGuest(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000"), Restart: models.RestartAlways, Backoff: time.Millisecond}); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitKind(t, c, supervisor.KindReady)

	if err := c.StopApp(t.Context(), false); err != nil {
		t.Fatalf("stop the app: %v", err)
	}
	exit := awaitKind(t, c, supervisor.KindExit)
	if exit.Exit == nil || exit.Exit.Signal != int(syscall.SIGTERM) {
		t.Fatalf("exit = %+v, want signal SIGTERM", exit.Exit)
	}
	restarts := awaitKind(t, c, supervisor.KindRestarts)
	if restarts.Restarts == nil || !restarts.Restarts.Ended || restarts.Restarts.Count != 0 {
		t.Fatalf("restarts = %+v, want the app ended with no start again", restarts.Restarts)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the guest ended with its app: %v", err)
	}
}

// treeArgv is sh -c 'sleep 600 & wait', which leaves its sleep behind on a TERM, and writes the sleep's pid to pidFile.
func treeArgv(pidFile, then string) []string {
	return []string{"/bin/sh", "-c", `sleep 600 & echo $! >>"$1"; ` + then, "sh", pidFile}
}

// forkedPIDs reads the sleeps a tree app forked, one per run, once n of them are there.
func forkedPIDs(t *testing.T, pidFile string, n int) []int {
	t.Helper()

	var pids []int
	waitFor(t, 10*time.Second, fmt.Sprintf("%d forked sleeps", n), func() bool {
		raw, err := os.ReadFile(pidFile)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil {
			t.Fatalf("read the forked pids: %v", err)
		}
		pids = pids[:0]
		for field := range strings.FieldsSeq(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return false
			}
			pids = append(pids, pid)
		}

		return len(pids) >= n
	})

	return pids
}

// gone says a pid ended: no such process, or a zombie its new parent has yet to reap.
func gone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))

	return err == nil && strings.Contains(string(stat), ") Z ")
}

// app/stop signals the app's whole group, so what it forked ends too, while PID 1 and an exec session run on.
func TestTransportStopAppEndsWhatTheAppForked(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			cmd, dial := startTransport(t)
			ctx := testContext(t)
			c, err := supervisor.Connect(ctx, dial)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			pidFile := filepath.Join(shortDir(t), "forked")
			if err := c.Run(t.Context(), supervisor.RunSpec{Argv: treeArgv(pidFile, "wait"), Env: os.Environ()}); err != nil {
				t.Fatalf("run: %v", err)
			}
			forked := forkedPIDs(t, pidFile, 1)[0]
			t.Cleanup(func() { _ = syscall.Kill(forked, syscall.SIGKILL) })

			execCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			started := make(chan int, 1)
			execDone := make(chan error, 1)
			go func() {
				_, err := supervisor.Exec(execCtx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("sleep:60000")}, models.ExecSpec{Report: func(p int) { started <- p }})
				execDone <- err
			}()
			execPID := <-started

			if err := c.StopApp(t.Context(), force); err != nil {
				t.Fatalf("stop the app: %v", err)
			}
			if restarts := awaitKind(t, c, supervisor.KindRestarts); restarts.Restarts == nil || !restarts.Restarts.Ended {
				t.Fatalf("restarts = %+v, want the app ended", restarts.Restarts)
			}
			waitFor(t, 10*time.Second, fmt.Sprintf("the sleep the app forked, pid %d, to end", forked), func() bool { return gone(forked) })

			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the guest ended with its app: %v", err)
			}
			if err := syscall.Kill(execPID, 0); err != nil {
				t.Fatalf("the exec session's pid %d ended with the app: %v", execPID, err)
			}
			select {
			case err := <-execDone:
				t.Fatalf("the exec session ended with %v, want it attached", err)
			default:
			}
		})
	}
}

// An app that ends leaves nothing in its group, even a child that ignored the TERM, so no verb has to reach it after the end.
func TestTransportAnEndedAppLeavesNothingInItsGroup(t *testing.T) {
	cases := []struct {
		name string
		argv func(pidFile string) []string
		stop bool
	}{
		{name: "exit", argv: func(pidFile string) []string { return treeArgv(pidFile, "exit 0") }},
		{name: "stop", stop: true, argv: func(pidFile string) []string {
			return []string{"/bin/sh", "-c", `(trap "" TERM; exec sleep 600) & echo $! >>"$1"; wait`, "sh", pidFile}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, dial := startTransport(t)
			ctx := testContext(t)
			c, err := supervisor.Connect(ctx, dial)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()
			pidFile := filepath.Join(shortDir(t), "forked")
			if err := c.Run(t.Context(), supervisor.RunSpec{Argv: tc.argv(pidFile), Env: os.Environ()}); err != nil {
				t.Fatalf("run: %v", err)
			}
			forked := forkedPIDs(t, pidFile, 1)[0]
			t.Cleanup(func() { _ = syscall.Kill(forked, syscall.SIGKILL) })
			end := ended(c)

			if tc.stop {
				if err := c.StopApp(t.Context(), false); err != nil {
					t.Fatalf("stop the app: %v", err)
				}
			}
			select {
			case <-end:
			case <-time.After(10 * time.Second):
				t.Fatal("the app never ended")
			}
			waitFor(t, 10*time.Second, fmt.Sprintf("the child the app left, pid %d, to end", forked), func() bool { return gone(forked) })
		})
	}
}

// A start again never runs beside what the last run left behind, so a restart kills the last run's group.
func TestTransportARestartEndsWhatTheLastRunForked(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	pidFile := filepath.Join(shortDir(t), "forked")
	spec := supervisor.RunSpec{Argv: treeArgv(pidFile, "exit 1"), Env: os.Environ(), Restart: models.RestartOnFailure, Retries: 1, Backoff: time.Millisecond}
	if err := c.Run(t.Context(), spec); err != nil {
		t.Fatalf("run: %v", err)
	}
	forked := forkedPIDs(t, pidFile, 2)
	t.Cleanup(func() {
		for _, pid := range forked {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	waitFor(t, 10*time.Second, fmt.Sprintf("the first run's sleep, pid %d, to end", forked[0]), func() bool { return gone(forked[0]) })
}

// A kill of a guest whose entrypoint already exited ends nothing, so a host lost before the cut must read the freeze off the replay and thaw it (SHARD-344).
func TestTransportKillReplaysFrozenOnTheNextHost(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// The entrypoint exits, so the kill finds none to forward to and the guest stays up for the cut.
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("exit:0")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitKind(t, c, supervisor.KindExit)
	if err := c.Kill(t.Context()); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = c.Close()

	next, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer next.Close()
	if state := awaitKind(t, next, supervisor.KindState); !state.Frozen {
		t.Fatal("the replay after a kill says not frozen, so the next host would strand it")
	}

	// A stop still ends the guest the kill left frozen.
	if err := next.Stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the supervisor ended with %v, want a clean exit", err)
		}
	case <-ctx.Done():
		t.Fatal("the supervisor did not exit after the stop")
	}
}

// writeWatch says when the guest starts a write, which net.Pipe then holds until the host reads.
type writeWatch struct {
	net.Conn
	writing chan struct{}
}

func (w *writeWatch) Write(b []byte) (int, error) {
	select {
	case w.writing <- struct{}{}:
	default:
	}

	return w.Conn.Write(b)
}

// A guest with nothing to forward a stop to goes at once, so the host must hold its answer first (SHARD-483).
func TestTransportAnswersAStopBeforeTheGuestActsOnIt(t *testing.T) {
	ctx := testContext(t)
	host, conn := net.Pipe()
	t.Cleanup(func() { _ = host.Close() })
	guest := &writeWatch{Conn: conn, writing: make(chan struct{}, 1)}
	tr := &transport{attached: make(chan struct{}, 1), control: guest}
	tr.g = newGuest(tr, restartPolicy{})
	go tr.serveControl(guest)

	if err := supervisor.WriteMessage(host, supervisor.Message{Kind: supervisor.KindStop, ID: 1}); err != nil {
		t.Fatalf("send the stop: %v", err)
	}
	select {
	case <-guest.writing:
	case <-ctx.Done():
		t.Fatal("the guest never started its answer to the stop")
	}
	// The runtime hands on a pending SIGTERM before a later, higher SIGWINCH, so any stop the guest signalled itself is in by now.
	fence := make(chan os.Signal, 1)
	signal.Notify(fence, syscall.SIGWINCH)
	t.Cleanup(func() { signal.Stop(fence) })
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("send the fence: %v", err)
	}
	select {
	case <-fence:
	case <-ctx.Done():
		t.Fatal("the fence signal never arrived")
	}
	select {
	case <-tr.g.stopSignals:
		t.Fatal("the stop reached the guest while the host had not read its answer")
	default:
	}

	var answer supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(host), &answer); err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if answer.Kind != supervisor.KindDone || answer.ID != 1 {
		t.Fatalf("answer = %+v, want done for request 1", answer)
	}
	select {
	case <-tr.g.stopSignals:
	case <-ctx.Done():
		t.Fatal("the guest never acted on the stop it answered")
	}
}

// A pause waits on the freeze, and the host that restores the checkpoint reads the frozen root off the replay and thaws it.
func TestTransportFreezeAndThawReplayOnTheNextHost(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)
	attach := func(wantFrozen bool) *supervisor.Control {
		t.Helper()
		c, err := supervisor.Connect(ctx, dial)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if state := awaitKind(t, c, supervisor.KindState); state.Frozen != wantFrozen {
			t.Fatalf("the replay says frozen %t, want %t", state.Frozen, wantFrozen)
		}

		return c
	}

	c := attach(false)
	for range 2 {
		if err := c.Freeze(t.Context(), models.VerbPause); err != nil {
			t.Fatalf("freeze: %v", err)
		}
	}
	c = attach(true)
	for range 2 {
		if err := c.Thaw(t.Context()); err != nil {
			t.Fatalf("thaw: %v", err)
		}
	}
	c = attach(false)

	// A stop still ends a frozen guest.
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := c.Freeze(t.Context(), models.VerbPause); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitKind(t, c, supervisor.KindExit)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the supervisor ended with %v, want a clean exit", err)
		}
	case <-ctx.Done():
		t.Fatal("the supervisor did not exit after the stop")
	}
}

func TestTransportExecWithNoStdinSeesEOF(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The echo role copies stdin until EOF; with no stdin it must still end, and promptly.
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	exit, err := supervisor.Exec(execCtx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("echo:0"), Env: os.Environ()}, models.ExecSpec{})
	if err != nil {
		t.Fatalf("exec with no stdin: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %+v, want code 0", exit)
	}
}

func TestTransportRefusedExecsLeakNoDescriptor(t *testing.T) {
	// The supervisor is undumpable, so its fd table cannot be counted; a limit it must stay under shows a leak instead.
	_, dial := startTransportUnder(t, 48)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	for i := range 40 {
		_, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"/nonexistent/cmd"}}, models.ExecSpec{})
		var notStarted *models.CommandNotStartedError
		if !errors.As(err, &notStarted) {
			t.Fatalf("refused exec %d gave %v, want CommandNotStartedError", i, err)
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	exit, err := supervisor.Exec(execCtx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("say:still-here"), Env: os.Environ()}, models.ExecSpec{})
	if err != nil || exit.Code != 0 {
		t.Fatalf("after 40 refused execs a healthy one gave %+v, %v", exit, err)
	}
}

func TestAnswerStaysOnTheConnectionThatAsked(t *testing.T) {
	oldHost, oldGuest := net.Pipe()
	newHost, newGuest := net.Pipe()
	defer oldHost.Close()
	defer newHost.Close()
	tr := &transport{control: newGuest}

	// The old host asked, the new one replaced it; the old answer must reach neither.
	tr.answer(oldGuest, 1, nil)
	_ = newHost.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var stray supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(newHost), &stray); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the new host read %+v (%v), want nothing", stray, err)
	}

	go tr.answer(newGuest, 2, nil)
	_ = newHost.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(newHost), &reply); err != nil || reply.ID != 2 || reply.Kind != supervisor.KindDone {
		t.Fatalf("the new host read %+v (%v), want done 2", reply, err)
	}
}

// A freeze whose host was replaced before the answer is undone, since the new host's replay may have read the root before it froze.
func TestAFreezeNoHostHeardIsUndone(t *testing.T) {
	_, oldGuest := net.Pipe()
	newHost, nextGuest := net.Pipe()
	defer oldGuest.Close()
	defer newHost.Close()
	tr := &transport{control: nextGuest}
	tr.g = newGuest(tr, restartPolicy{})
	serveCommands(t, tr.g)

	tr.freeze(oldGuest, 1, models.VerbFork)
	if tr.g.frozen.Load() != nil {
		t.Fatal("the root stays frozen after a freeze no host heard")
	}

	go tr.freeze(nextGuest, 2, models.VerbFork)
	_ = newHost.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(newHost), &reply); err != nil || reply.ID != 2 || reply.Kind != supervisor.KindDone {
		t.Fatalf("the new host read %+v (%v), want done 2", reply, err)
	}
	if verb := tr.g.frozen.Load(); verb == nil || *verb != models.VerbFork {
		t.Fatal("the root is not held by the fork after a freeze its host heard")
	}
}

// serveCommands runs a guest's commands as its loop does, for a test that drives the transport without a supervise.
func serveCommands(t *testing.T, g *guest) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case command := <-g.commands:
				command()
			case <-stop:
				return
			}
		}
	}()
}

// An exec while a fork holds the guest frozen is refused by name, since a child forked into the frozen bound would hold the loop the thaw needs (SHARD-462).
func TestTransportRefusesAnExecWhileFrozen(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := c.Freeze(t.Context(), models.VerbFork); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	_, err = supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("exit:0")}, models.ExecSpec{})
	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) || !strings.Contains(err.Error(), "sandbox sb could not run the command: a fork holds the sandbox frozen") {
		t.Fatalf("exec while frozen gave %v, want the refusal that names the fork", err)
	}

	if err := c.Thaw(t.Context()); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if _, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("exit:0")}, models.ExecSpec{}); err != nil {
		t.Fatalf("exec after the thaw: %v", err)
	}
}

// A restart due while the guest is frozen waits for the thaw, since a refused restart would give up for good.
func TestTransportRestartWaitsOutAFreeze(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:300"), Restart: models.RestartAlways, Backoff: 10 * time.Millisecond}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Off a VM nothing really freezes, so the entrypoint exits under the freeze and its restart falls due there.
	if err := c.Freeze(t.Context(), models.VerbFork); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	awaitKind(t, c, supervisor.KindExit)
	time.Sleep(200 * time.Millisecond)
	if err := c.Thaw(t.Context()); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	restarts := awaitKind(t, c, supervisor.KindRestarts)
	if restarts.Restarts == nil || restarts.Restarts.GaveUp || restarts.Restarts.Count != 1 {
		t.Fatalf("the restart after the thaw reported %+v, want count 1 and no give-up", restarts.Restarts)
	}
}

// bootFailure runs failBoot over unix sockets, as a guest whose mounts failed before any listener was up.
func bootFailure(t *testing.T, cause error) (<-chan error, supervisor.Dialer) {
	t.Helper()
	dir := shortDir(t)
	listen, err := listenerFor("unix:" + dir)
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- failBoot(listen, fmt.Errorf("%w: %w", errSupervisor, cause)) }()

	return failed, func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	}
}

// A boot that fails before the listeners exist tells the host that dials, so a start answers with the reason, not a 30s timeout (SHARD-416).
func TestABootFailureOpensTheControlConnectionWithTheDeath(t *testing.T) {
	cause := errors.New("mount /dev/vdb on /overlay: read-only file system")
	failed, dial := bootFailure(t, cause)

	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Next()
	if err != nil {
		t.Fatalf("read the opening message: %v", err)
	}
	if m.Kind != supervisor.KindSupervisorFailed || m.Exit == nil || m.Exit.Code != models.SupervisorFailedExitCode || !strings.Contains(m.Error, cause.Error()) {
		t.Fatalf("the host read %+v, want supervisor-failed with code 125 and the cause", m)
	}

	// The halt follows the exit at once, so the guest holds on until the host hangs up.
	select {
	case err := <-failed:
		t.Fatalf("failBoot returned %v while the host was still attached", err)
	case <-time.After(100 * time.Millisecond):
	}
	c.Close()
	err = <-failed
	if !errors.Is(err, errSupervisor) || err.Error() != fmt.Errorf("%w: %w", errSupervisor, cause).Error() {
		t.Fatalf("failBoot returned %v, want only the supervisor error and its cause", err)
	}
	if exitCodeFor(err) != models.SupervisorFailedExitCode {
		t.Fatalf("the exit code is %d, want 125", exitCodeFor(err))
	}
}

func TestABootFailureGivesUpWhenNoHostComes(t *testing.T) {
	old := failureGrace
	failureGrace = 100 * time.Millisecond
	t.Cleanup(func() { failureGrace = old })

	failed, _ := bootFailure(t, errors.New("unshare the cgroup namespace: operation not permitted"))
	err := <-failed
	if !errors.Is(err, errSupervisor) || !strings.Contains(err.Error(), "no host attached") {
		t.Fatalf("failBoot returned %v, want the supervisor error and no host", err)
	}
	if exitCodeFor(err) != models.SupervisorFailedExitCode {
		t.Fatalf("the exit code is %d, want 125", exitCodeFor(err))
	}
}

func TestABootFailureGivesUpOnAHostThatNeverHangsUp(t *testing.T) {
	old := failureGrace
	failureGrace = 200 * time.Millisecond
	t.Cleanup(func() { failureGrace = old })

	failed, dial := bootFailure(t, errors.New("chroot onto the root disk: no such file or directory"))
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if m, err := c.Next(); err != nil || m.Kind != supervisor.KindSupervisorFailed {
		t.Fatalf("the host read %+v, %v, want supervisor-failed", m, err)
	}
	err = <-failed
	if !errors.Is(err, errSupervisor) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("failBoot returned %v, want the supervisor error and the wait that ran out", err)
	}
}

func TestSealRootCarriesTheFreezeError(t *testing.T) {
	refused := errors.New("freeze the root: operation not supported")
	if err := sealRoot(nil, func(*os.File) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("sealRoot returned %v, want the freeze error", err)
	}
	if err := sealRoot(nil, func(*os.File) error { return nil }); err != nil {
		t.Fatalf("sealRoot returned %v, want nil", err)
	}
}

func TestSealSkipsTheFreezeOfAForcedStop(t *testing.T) {
	froze := 0
	freeze := func(*os.File) error { froze++; return nil }
	tr := &transport{}
	if err := tr.seal(freeze); err != nil || froze != 1 {
		t.Fatalf("a clean stop's seal returned %v after %d freezes, want nil after 1", err, froze)
	}
	tr.forced.Store(true)
	if err := tr.seal(freeze); err != nil || froze != 1 {
		t.Fatalf("a forced stop's seal returned %v after %d freezes, want nil and no new freeze", err, froze)
	}
}

func TestARecoveredKillSealsALaterCleanStop(t *testing.T) {
	froze := 0
	freeze := func(*os.File) error { froze++; return nil }
	tr := &transport{}
	tr.g = newGuest(tr, restartPolicy{})

	tr.forced.Store(true)
	if err := tr.handle(supervisor.Message{Kind: supervisor.KindThaw}); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if err := tr.seal(freeze); err != nil || froze != 1 {
		t.Fatalf("a seal after a thawed kill returned %v after %d freezes, want nil after 1", err, froze)
	}

	stale, _ := net.Pipe()
	tr.forced.Store(true)
	tr.stop(stale, 1)
	<-tr.g.stopSignals
	if err := tr.seal(freeze); err != nil || froze != 2 {
		t.Fatalf("a clean stop after an unheard kill returned %v after %d freezes, want nil after 2", err, froze)
	}
}

func TestSealRootGivesUpOnAFreezeThatHangs(t *testing.T) {
	old := sealGrace
	sealGrace = 50 * time.Millisecond
	t.Cleanup(func() { sealGrace = old })

	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	err := sealRoot(nil, func(*os.File) error { <-hung; return nil })
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("sealRoot returned %v, want the freeze bound", err)
	}
}
