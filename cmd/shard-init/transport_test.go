package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	dir := shortDir(t)
	cmd := exec.Command(exe, "-transport", "unix:"+dir)
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

	if err := c.Run(supervisor.RunSpec{Argv: childArgv("exit:7"), Env: os.Environ()}); err != nil {
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
	if state.Kind != supervisor.KindState || !state.Ready || state.Exit == nil || state.Exit.Code != 7 {
		t.Fatalf("state = %+v, want ready with exit code 7", state)
	}
	if err := again.Run(supervisor.RunSpec{Argv: childArgv("exit:0")}); !errors.Is(err, supervisor.ErrEntrypointNotStarted) {
		t.Fatalf("a second run gave %v, want ErrEntrypointNotStarted", err)
	}
}

func TestTransportRunFailureNamesTheBinary(t *testing.T) {
	_, dial := startTransport(t)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	err = c.Run(supervisor.RunSpec{Argv: []string{"/nonexistent/entrypoint"}})
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
	if err := c.Run(supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
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
	if err := c.Run(supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
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
	if err := c.Run(supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	if err := c.Signal(os.Getpid(), "KILL"); err != nil {
		t.Fatalf("send the signal: %v", err)
	}
	failure := awaitKind(t, c, supervisor.KindFailure)
	if !strings.Contains(failure.Error, "not a process shard-init started") {
		t.Fatalf("failure = %q, want the refusal", failure.Error)
	}
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
	if err := c.Run(supervisor.RunSpec{Argv: childArgv("say:first line")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitKind(t, c, supervisor.KindExit)

	logsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- supervisor.Logs(logsCtx, dial, &logs) }()
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
	if err := c.Run(supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := c.Stop(); err != nil {
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
