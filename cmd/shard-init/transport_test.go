package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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

func TestTransportExecNotStartedReports127(t *testing.T) {
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

	_, err = supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"/nonexistent/cmd"}}, models.ExecSpec{})
	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) || notStarted.Code != 127 {
		t.Fatalf("exec gave %v, want CommandNotStartedError with code 127", err)
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

// A pause waits on the freeze, and the host that restores the snapshot reads the frozen root off the replay and thaws it.
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
		if err := c.Freeze(t.Context()); err != nil {
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
	if err := c.Freeze(t.Context()); err != nil {
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
	newHost, newGuest := net.Pipe()
	defer oldGuest.Close()
	defer newHost.Close()
	tr := &transport{control: newGuest}

	tr.freeze(oldGuest, 1)
	if tr.frozen.Load() {
		t.Fatal("the root stays frozen after a freeze no host heard")
	}

	go tr.freeze(newGuest, 2)
	_ = newHost.SetReadDeadline(time.Now().Add(5 * time.Second))
	var reply supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(newHost), &reply); err != nil || reply.ID != 2 || reply.Kind != supervisor.KindDone {
		t.Fatalf("the new host read %+v (%v), want done 2", reply, err)
	}
	if !tr.frozen.Load() {
		t.Fatal("the root is not frozen after a freeze its host heard")
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
