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
	"slices"
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

// supervisorProcess is a shard-init a test started, which the test and its cleanup may both wait for.
type supervisorProcess struct {
	*exec.Cmd
	once    sync.Once
	waitErr error
}

func (p *supervisorProcess) wait() error {
	p.once.Do(func() { p.waitErr = p.Wait() })

	return p.waitErr
}

// startTransport runs this binary as shard-init -transport unix:<dir>, the vsock mode over unix sockets.
func startTransport(t *testing.T) (*supervisorProcess, supervisor.Dialer) {
	t.Helper()

	return startTransportUnder(t, 0)
}

// startTransportUnder is the same with a descriptor limit on the supervisor; zero keeps the test's own.
func startTransportUnder(t *testing.T, limit uint64) (*supervisorProcess, supervisor.Dialer) {
	t.Helper()

	dir := shortDir(t)
	cmd := exec.Command(selfBinary, "-transport", "unix:"+dir)
	if limit > 0 {
		// The hard limit too, or the Go runtime raises the soft one back at startup; the shell keeps the pid.
		cmd = exec.Command("/bin/sh", "-c", `ulimit -n "$1" && exec "$2" -transport "$3"`, "sh", strconv.FormatUint(limit, 10), selfBinary, "unix:"+dir)
	}
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)
	cmd.Stderr = os.Stderr
	p := startSupervisor(t, cmd)

	// The logs socket is the last the guest binds, so once it is there an exec dial finds its listener.
	waitFor(t, 10*time.Second, "the supervisor to listen", func() bool {
		_, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.LogsPort)))

		return err == nil
	})
	dial := func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	}

	return p, dial
}

// startSupervisor starts cmd, and kills it once the test ends unless the test saw it exit.
func startSupervisor(t *testing.T, cmd *exec.Cmd) *supervisorProcess {
	t.Helper()

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}
	p := &supervisorProcess{Cmd: cmd}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}
		// The kill is what a supervisor still up ends with.
		if err := p.wait(); err != nil {
			if _, exited := errors.AsType[*exec.ExitError](err); !exited {
				t.Errorf("wait for the supervisor: %v", err)
			}
		}
	})

	return p
}

// awaitCleanExit waits for the supervisor to end with code 0 after what the test asked of it.
func awaitCleanExit(ctx context.Context, t *testing.T, p *supervisorProcess, after string) {
	t.Helper()

	waited := make(chan error, 1)
	go func() { waited <- p.wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the supervisor ended with %v after %s, want a clean exit", err, after)
		}
	case <-ctx.Done():
		t.Fatalf("the supervisor did not exit after %s", after)
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func childArgv(role string) []string {
	return []string{selfBinary, childPrefix + role}
}

// closeLater closes c once the test ends; one the test or the guest closed already is fine.
func closeLater(t *testing.T, c io.Closer) {
	t.Helper()

	t.Cleanup(func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("close: %v", err)
		}
	})
}

// killLater kills a pid a test's sandbox left, once the test ends; one already gone is fine.
func killLater(t *testing.T, pid int) {
	t.Helper()

	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill pid %d: %v", pid, err)
		}
	})
}

// connect attaches as a host does.
func connect(ctx context.Context, t *testing.T, dial supervisor.Dialer) *supervisor.Control {
	t.Helper()

	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	closeLater(t, c)

	return c
}

// connectReady attaches and hands the guest its setup, as a host does once per boot before any run.
func connectReady(ctx context.Context, t *testing.T, dial supervisor.Dialer) *supervisor.Control {
	t.Helper()

	c := connect(ctx, t, dial)
	if err := c.Setup(ctx, supervisor.Setup{}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	return c
}

// awaitKind reads c's events until one of kind; a guest that never sends it fails the test rather than hang it.
func awaitKind(t *testing.T, c *supervisor.Control, kind string) supervisor.Message {
	t.Helper()

	deadline := time.After(20 * time.Second)
	for {
		got := make(chan supervisor.Message, 1)
		failed := make(chan error, 1)
		go func() {
			m, err := c.Next()
			if err != nil {
				failed <- err

				return
			}
			got <- m
		}()
		select {
		case m := <-got:
			if m.Kind == kind {
				return m
			}
		case err := <-failed:
			t.Fatalf("read the control channel for a %s event: %v", kind, err)
		case <-deadline:
			t.Fatalf("no %s event within 20s", kind)
		}
	}
}

// awaitProcess reads c's events until name reaches state.
func awaitProcess(t *testing.T, c *supervisor.Control, name string, state models.ProcessState) models.ProcessReport {
	t.Helper()

	for {
		m := awaitKind(t, c, supervisor.KindProcess)
		if m.Process == nil {
			t.Fatalf("a process event carries no process: %+v", m)
		}
		if m.Process.Name == name && m.Process.State == state {
			return *m.Process
		}
	}
}

// endShown closes once c hears name end.
func endShown(c *supervisor.Control, name string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for {
			m, err := c.Next()
			if err != nil {
				return
			}
			if m.Kind == supervisor.KindProcess && m.Process != nil && m.Process.Name == name && m.Process.State.Ended() {
				close(done)

				return
			}
		}
	}()

	return done
}

func reportNames(reports []models.ProcessReport) []string {
	names := make([]string, 0, len(reports))
	for _, p := range reports {
		names = append(names, p.Name)
	}

	return names
}

// openLogs opens the logs of name as a host does and reads the output bytes [from, to) the guest holds of it.
func openLogs(ctx context.Context, t *testing.T, dial supervisor.Dialer, name string) (net.Conn, [2]uint64) {
	t.Helper()

	conn, err := dial(ctx, supervisor.LogsPort)
	if err != nil {
		t.Fatalf("open the logs connection: %v", err)
	}
	closeLater(t, conn)
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("bound the logs connection: %v", err)
	}
	if err := supervisor.WriteMessage(conn, supervisor.LogsOpen{Name: name}); err != nil {
		t.Fatalf("name the process %q: %v", name, err)
	}
	var held [2]uint64
	if err := binary.Read(conn, binary.BigEndian, &held); err != nil {
		t.Fatalf("read the output the guest holds of %q: %v", name, err)
	}

	return conn, held
}

// sendOffset is a host's resume or ack: the output byte its log file holds up to.
func sendOffset(t *testing.T, conn net.Conn, at uint64) {
	t.Helper()

	if err := binary.Write(conn, binary.BigEndian, at); err != nil {
		t.Fatalf("send the logs offset %d: %v", at, err)
	}
}

func readOutput(t *testing.T, conn net.Conn, n int) string {
	t.Helper()

	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes of output, got %q: %v", n, buf, err)
	}

	return string(buf)
}

// pipe is an os.Pipe whose two ends close once the test ends, and a second close is fine.
func pipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{r, w} {
		t.Cleanup(func() {
			if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Errorf("close %s: %v", f.Name(), err)
			}
		})
	}

	return r, w
}

// drain reads what an exec wrote into w, once the exec is over and the test let go of its own write end.
func drain(t *testing.T, r, w *os.File) string {
	t.Helper()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		t.Fatal(err)
	}

	return out.String()
}

// The first host hears the guest unready with an empty table; after the setup each run is reported, numbered in order, and a later host reads the table in that order.
func TestTransportReportsEachProcessAndReplaysTheTable(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)

	c := connect(ctx, t, dial)
	state := awaitKind(t, c, supervisor.KindState)
	if state.Ready || state.Version != supervisor.ProcessVersion || len(state.Processes) != 0 {
		t.Fatalf("the first state is %+v, want unready, version %d and no process", state, supervisor.ProcessVersion)
	}
	if err := c.Setup(ctx, supervisor.Setup{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, spec := range []supervisor.RunSpec{named("zeta", "sleep:60000"), named("alpha", "sleep:60000"), named("mid", "exit:7")} {
		if err := c.Run(ctx, spec); err != nil {
			t.Fatalf("run %s: %v", spec.Name, err)
		}
	}

	var last uint64
	for {
		m := awaitKind(t, c, supervisor.KindProcess)
		if m.Process.Seq <= last {
			t.Fatalf("%s reads seq %d after seq %d", m.Process.Name, m.Process.Seq, last)
		}
		last = m.Process.Seq
		if m.Process.Name != "mid" || !m.Process.State.Ended() {
			continue
		}
		if m.Process.State != models.ProcessExited || m.Process.Exit == nil || m.Process.Exit.Code != 7 {
			t.Fatalf("mid ended as %+v, want exited with code 7", m.Process)
		}

		break
	}

	// A second control connection, the way a restarted daemon comes back, hears the state first.
	again := connect(ctx, t, dial)
	state = awaitKind(t, again, supervisor.KindState)
	if !state.Ready || state.Version != supervisor.ProcessVersion || state.Logs != supervisor.LogsVersion {
		t.Fatalf("state = %+v, want ready, version %d and logs version %d", state, supervisor.ProcessVersion, supervisor.LogsVersion)
	}
	if names := reportNames(state.Processes); !slices.Equal(names, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("the replay holds %q, want them in the order they were numbered", names)
	}
	if mid := state.Processes[2]; mid.State != models.ProcessExited || mid.Seq != last {
		t.Fatalf("the replay holds mid as %+v, want its exit at seq %d", mid, last)
	}

	err := again.Run(ctx, named("zeta", "exit:0"))
	if refusal, ok := errors.AsType[*supervisor.Refusal](err); !ok || !refusal.Answer.Taken {
		t.Fatalf("a run of a live name gave %v, want a refusal that says taken", err)
	}
}

func TestTransportRunFailureNamesTheBinary(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)

	err := c.Run(ctx, supervisor.RunSpec{Name: "web", Argv: []string{"/nonexistent/entrypoint"}})
	refusal, ok := errors.AsType[*supervisor.Refusal](err)
	if !ok || refusal.Answer.Code != models.CommandNotFoundExitCode || !strings.Contains(err.Error(), "/nonexistent/entrypoint") {
		t.Fatalf("run gave %v, want a refusal with code 127 naming the binary", err)
	}
	// A run that never started holds no name.
	if err := c.Run(ctx, named("web", "exit:0")); err != nil {
		t.Fatalf("a run of the name after the failed one: %v", err)
	}
}

func TestTransportExecMovesTheStreams(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)

	stdinR, stdinW := pipe(t)
	stdoutR, stdoutW := pipe(t)
	stderrR, stderrW := pipe(t)
	wrote := make(chan error, 1)
	go func() {
		_, err := stdinW.WriteString("hello over vsock")
		wrote <- errors.Join(err, stdinW.Close())
	}()
	var pid int
	spec := models.ExecSpec{Stdin: stdinR, Stdout: stdoutW, Stderr: stderrW, Report: func(p int) { pid = p }}
	exit, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("echo:3"), Env: os.Environ()}, spec)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("write the stdin: %v", err)
	}
	if exit.Code != 3 {
		t.Fatalf("exit = %+v, want code 3", exit)
	}
	if pid <= 0 {
		t.Fatalf("pid = %d, want the started frame", pid)
	}
	if stdout, stderr := drain(t, stdoutR, stdoutW), drain(t, stderrR, stderrW); stdout != "hello over vsock" || stderr != "echo-err" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
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

			_, err := supervisor.Exec(testContext(t), dial, "sb", header, models.ExecSpec{})
			notStarted, ok := errors.AsType[*models.CommandNotStartedError](err)
			if !ok || notStarted.Code != models.CommandNotFoundExitCode {
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

	stdoutR, stdoutW := pipe(t)
	exit, err := supervisor.Exec(testContext(t), dial, "sb", supervisor.ExecHeader{Argv: []string{"/bin/sh", "-c", "umask"}}, models.ExecSpec{Stdout: stdoutW})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if umask := drain(t, stdoutR, stdoutW); exit.Code != 0 || umask != "0022\n" {
		t.Fatalf("exit %+v, umask %q, want 0 and 0022", exit, umask)
	}
}

// docker run -w makes a missing work directory, since every process and exec defaults to it (SHARD-757).
func TestTransportSetupMakesTheWorkDirectory(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connect(ctx, t, dial)
	dir := filepath.Join(shortDir(t), "work", "deep")

	if err := c.Setup(ctx, supervisor.Setup{WorkDir: dir}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the work directory after the setup: %v, want a directory", err)
	}
	if state := awaitKind(t, connect(ctx, t, dial), supervisor.KindState); !state.Ready {
		t.Fatalf("the replay after the setup = %+v, want ready", state)
	}
}

func TestTransportSetupRefusesAWorkDirectoryItCannotMake(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connect(ctx, t, dial)
	file := filepath.Join(shortDir(t), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := c.Setup(ctx, supervisor.Setup{WorkDir: filepath.Join(file, "work")})
	if err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("setup gave %v, want the refusal naming the work directory", err)
	}
	if state := awaitKind(t, connect(ctx, t, dial), supervisor.KindState); state.Ready {
		t.Fatal("the replay says ready after a setup that failed")
	}
}

// A process never makes its work directory, so one that is not there is the shell's 126 and names the directory.
func TestTransportARunRefusesAWorkDirectoryThatIsNotThere(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	dir := filepath.Join(shortDir(t), "missing")

	spec := named("web", "exit:0")
	spec.WorkDir = dir
	err := c.Run(ctx, spec)
	refusal, ok := errors.AsType[*supervisor.Refusal](err)
	if !ok || refusal.Answer.Code != models.CommandNotExecutableExitCode || !strings.Contains(err.Error(), strconv.Quote(dir)) {
		t.Fatalf("run gave %v, want a refusal with code 126 naming the work directory", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run made %s: %v", dir, err)
	}
}

// docker exec -w never makes the directory and answers 126; fork's chdir would name the binary instead (SHARD-757).
func TestTransportAnExecRefusesAWorkDirectoryAndNamesIt(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connect(ctx, t, dial)
	root := shortDir(t)
	work := filepath.Join(root, "work")
	if err := c.Setup(ctx, supervisor.Setup{WorkDir: work}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The sandbox's own work directory, removed after the setup, is the one every exec defaults to.
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
		notStarted, ok := errors.AsType[*models.CommandNotStartedError](err)
		if !ok || notStarted.Code != models.CommandNotExecutableExitCode {
			t.Fatalf("an exec in %s gave %v, want CommandNotStartedError with code 126", dir, err)
		}
		if !strings.Contains(notStarted.Reason, strconv.Quote(dir)+" "+want) || strings.Contains(notStarted.Reason, strconv.Quote(argv[0])) {
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
	c := connect(ctx, t, dial)

	err := c.Signal(ctx, os.Getpid(), "KILL")
	if err == nil || !strings.Contains(err.Error(), "not a process shard-init started") {
		t.Fatalf("signal gave %v, want the refusal", err)
	}
}

// pid 0 and a negative pid must never reach kill(2), where they name a process group that holds PID 1.
func TestTransportSignalRefusesAProcessGroup(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connect(ctx, t, dial)

	for _, pid := range []int{0, -1} {
		err := c.Signal(ctx, pid, "KILL")
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
	c := connect(ctx, t, dial)

	if err := c.Reseed(ctx); err != nil {
		t.Fatalf("reseed: %v", err)
	}
}

func TestTransportExecCancelKillsTheCommand(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)

	execCtx, cancel := context.WithCancel(ctx)
	started := make(chan int, 1)
	spec := models.ExecSpec{Report: func(p int) { started <- p }}
	done := make(chan error, 1)
	go func() {
		_, err := supervisor.Exec(execCtx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("sleep:60000")}, spec)
		done <- err
	}()
	var pid int
	select {
	case pid = <-started:
	case err := <-done:
		t.Fatalf("the exec ended before it started: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("exec gave %v, want context.Canceled", err)
	}
	// The supervisor reaps its child, so a signal of zero says ESRCH once the command is gone.
	waitFor(t, 10*time.Second, fmt.Sprintf("pid %d to end after the cancel", pid), func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

// A hang-up with no cancel frame is a daemon restart: the command runs on and its output drains, as on gVisor (SHARD-270).
func TestTransportExecOutlivesAHangUp(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)

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
	killLater(t, started.PID)
	if err := conn.Close(); err != nil {
		t.Fatalf("hang up: %v", err)
	}

	waitFor(t, 10*time.Second, "the command to get past its output after the hang-up", func() bool {
		_, err := os.Stat(marker)

		return err == nil
	})
	if err := syscall.Kill(started.PID, 0); err != nil {
		t.Fatalf("pid %d is gone after a hang-up: %v", started.PID, err)
	}
}

// A process speaks before any logs connection exists, so its bytes wait for the host that opens its logs.
func TestTransportLogsFollowANamedProcess(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	if err := c.Run(ctx, named("web", "say:first line")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "web", models.ProcessRunning)

	logs, held := openLogs(ctx, t, dial, "web")
	if held[0] != 0 {
		t.Fatalf("the guest holds %v, want the output from byte 0", held)
	}
	sendOffset(t, logs, held[0])
	if line := readOutput(t, logs, len("first line\n")); line != "first line\n" {
		t.Fatalf("logs = %q, want the process's line", line)
	}
}

// A host that names a process the guest does not hold is hung up on before any header, which a host reads as no such process.
func TestTransportLogsOfAnUnknownProcessAreClosed(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	if err := c.Run(ctx, named("web", "sleep:60000")); err != nil {
		t.Fatalf("run: %v", err)
	}

	conn, err := dial(ctx, supervisor.LogsPort)
	if err != nil {
		t.Fatalf("open the logs connection: %v", err)
	}
	closeLater(t, conn)
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.WriteMessage(conn, supervisor.LogsOpen{Name: "nobody"}); err != nil {
		t.Fatalf("name the process: %v", err)
	}
	var held [2]uint64
	if err := binary.Read(conn, binary.BigEndian, &held); !errors.Is(err, io.EOF) {
		t.Fatalf("the logs of an unknown process read %v, %v; want a hang-up", held, err)
	}
}

// A fake pump takes the process's last output and acks it late, the way a host still writing its log file would; the end waits for that ack.
func TestTransportEndWaitsForTheHostToLandTheLastOutput(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)

	if err := c.Run(ctx, named("web", "say:last words")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "web", models.ProcessRunning)
	pump, held := openLogs(ctx, t, dial, "web")
	sendOffset(t, pump, held[0])
	tail := readOutput(t, pump, len("last words\n"))
	if tail != "last words\n" {
		t.Fatalf("the pump read %q, want the process's last line", tail)
	}
	select {
	case <-endShown(c, "web"):
		t.Fatal("the guest reported the end before the host acked the last output")
	case <-time.After(500 * time.Millisecond):
	}

	// A host that attaches meanwhile hears the process still running, so it waits for the same ack.
	again := connect(ctx, t, dial)
	state := awaitKind(t, again, supervisor.KindState)
	if len(state.Processes) != 1 || state.Processes[0].State != models.ProcessRunning {
		t.Fatalf("the replay holds %+v, want web still running", state.Processes)
	}

	sendOffset(t, pump, held[0]+uint64(len(tail)))
	if exited := awaitProcess(t, again, "web", models.ProcessExited); exited.Exit == nil || exited.Exit.Code != 0 {
		t.Fatalf("web ended as %+v, want exited with code 0", exited)
	}
}

// A host that comes back already holding the last output has no write to ack, so its resume is what lets the end go.
func TestTransportEndFollowsAHostThatResumesPastTheLastOutput(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)

	if err := c.Run(ctx, named("web", "say:last words")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "web", models.ProcessRunning)
	first, held := openLogs(ctx, t, dial, "web")
	sendOffset(t, first, held[0])
	tail := readOutput(t, first, len("last words\n"))
	end := endShown(c, "web")
	// The host lands the line and dies before its ack.
	if err := first.Close(); err != nil {
		t.Fatalf("close the first logs connection: %v", err)
	}

	second, held := openLogs(ctx, t, dial, "web")
	if held[1]-held[0] != uint64(len(tail)) {
		t.Fatalf("the guest holds %v, want the %d bytes of the unacked line", held, len(tail))
	}
	sendOffset(t, second, held[1])
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
	c := connectReady(ctx, t, dial)

	if err := c.Run(ctx, named("web", "say:last words")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "web", models.ProcessRunning)
	pump, held := openLogs(ctx, t, dial, "web")
	sendOffset(t, pump, held[0])
	readOutput(t, pump, len("last words\n"))
	end := endShown(c, "web")
	sendOffset(t, pump, supervisor.LogsStopped)
	select {
	case <-end:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest never reported the end after the host said its log stopped")
	}
	if _, err := io.Copy(io.Discard, pump); err != nil {
		t.Fatalf("the guest kept the logs connection open after the stop: %v", err)
	}
}

// A run of an ended name writes on in the same log, so a host following it reads the second run after the first.
func TestTransportARunOfAnEndedNameGoesOnInItsLog(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)

	if err := c.Run(ctx, named("job", "say:first")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "job", models.ProcessRunning)
	logs, held := openLogs(ctx, t, dial, "job")
	sendOffset(t, logs, held[0])
	if out := readOutput(t, logs, len("first\n")); out != "first\n" {
		t.Fatalf("the first run wrote %q", out)
	}
	sendOffset(t, logs, held[0]+uint64(len("first\n")))
	awaitProcess(t, c, "job", models.ProcessExited)

	if err := c.Run(ctx, named("job", "say:second")); err != nil {
		t.Fatalf("run again: %v", err)
	}
	if out := readOutput(t, logs, len("second\n")); out != "second\n" {
		t.Fatalf("the second run wrote %q on the same logs connection", out)
	}
}

// A name the guest let go of closes its sink and its host's logs connection, and a later process of the name numbers its output on from there, so a host's cursor never skips.
func TestTransportKeepClosesTheSinkOfANameLetGo(t *testing.T) {
	tr := &transport{shown: map[string]models.ProcessReport{}, sinks: map[string]*logSink{}, ends: map[string]uint64{}}
	t.Cleanup(func() { tr.keep(nil) })
	tr.keep([]string{"web"})
	out, err := tr.output("web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	sink := tr.sinks["web"]
	waitFor(t, 5*time.Second, "the sink to hold the output", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()

		return len(sink.held) == len("hello\n")
	})
	if err := tr.show(models.ProcessReport{Name: "web", Seq: 1}); err != nil {
		t.Fatal(err)
	}

	host, guestEnd := net.Pipe()
	closeLater(t, host)
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	sink.take(guestEnd)
	var held [2]uint64
	if err := binary.Read(host, binary.BigEndian, &held); err != nil || held != [2]uint64{0, 6} {
		t.Fatalf("the host was offered %v, %v; want bytes 0 to 6", held, err)
	}

	tr.keep(nil)
	if _, err := host.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the host's logs connection read %v once the name was let go of, want a hang-up", err)
	}
	if _, err := out.WriteString("late"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("a write to the pipe of a name let go of gave %v, want it closed", err)
	}
	if _, shown := tr.shown["web"]; shown || tr.ends["web"] != 6 {
		t.Fatalf("after the keep the replay holds web %t and its output ends at %d, want gone and 6", shown, tr.ends["web"])
	}

	tr.keep([]string{"web"})
	if _, err := tr.output("web"); err != nil {
		t.Fatal(err)
	}
	if from := tr.sinks["web"].from; from != 6 {
		t.Fatalf("the next process of the name starts its output at %d, want 6", from)
	}
}

func TestTransportStopEndsTheSupervisor(t *testing.T) {
	proc, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	if err := c.Run(ctx, named("web", "sleep:60000")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitCleanExit(ctx, t, proc, "the stop")
}

// With nothing to forward the stop to, the guest has nothing to wait for.
func TestTransportStopWithNothingRunningEndsAtOnce(t *testing.T) {
	proc, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	if err := c.Run(ctx, named("once", "exit:0")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "once", models.ProcessExited)

	if err := c.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitCleanExit(ctx, t, proc, "the stop")
}

// treeArgv is sh -c 'sleep 600 & wait', which leaves its sleep behind on a TERM, and writes the sleep's pid to pidFile.
func treeArgv(pidFile, then string) []string {
	return []string{"/bin/sh", "-c", `sleep 600 & echo $! >>"$1"; ` + then, "sh", pidFile}
}

// forkedPIDs reads the sleeps a tree process forked, one per run, once n of them are there.
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
	for _, pid := range pids {
		killLater(t, pid)
	}

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

func treeSpec(name, pidFile, then string) supervisor.RunSpec {
	return supervisor.RunSpec{Name: name, Argv: treeArgv(pidFile, then), Env: os.Environ()}
}

// A kill is the forced stop: it SIGKILLs every process's group, and the reap ends the guest.
func TestTransportKillForcesEveryProcessDown(t *testing.T) {
	proc, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	pidFile := filepath.Join(shortDir(t), "forked")
	if err := c.Run(ctx, treeSpec("tree", pidFile, "wait")); err != nil {
		t.Fatalf("run: %v", err)
	}
	forked := forkedPIDs(t, pidFile, 1)[0]

	if err := c.Kill(ctx); err != nil {
		t.Fatalf("kill: %v", err)
	}
	awaitCleanExit(ctx, t, proc, "the kill")
	waitFor(t, 10*time.Second, fmt.Sprintf("the sleep the process forked, pid %d, to end", forked), func() bool { return gone(forked) })
}

// A stop of one process signals its whole group, so what it forked ends too, while PID 1 and an exec session run on.
func TestTransportStopProcessEndsWhatItForkedAndKeepsTheGuest(t *testing.T) {
	for grace, sig := range map[time.Duration]syscall.Signal{10 * time.Second: syscall.SIGTERM, 0: syscall.SIGKILL} {
		t.Run(fmt.Sprintf("grace=%s", grace), func(t *testing.T) {
			proc, dial := startTransport(t)
			ctx := testContext(t)
			c := connectReady(ctx, t, dial)
			pidFile := filepath.Join(shortDir(t), "forked")
			if err := c.Run(ctx, treeSpec("tree", pidFile, "wait")); err != nil {
				t.Fatalf("run: %v", err)
			}
			forked := forkedPIDs(t, pidFile, 1)[0]

			execCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			started := make(chan int, 1)
			execDone := make(chan error, 1)
			go func() {
				_, err := supervisor.Exec(execCtx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("sleep:60000")}, models.ExecSpec{Report: func(p int) { started <- p }})
				execDone <- err
			}()
			var execPID int
			select {
			case execPID = <-started:
			case err := <-execDone:
				t.Fatalf("the exec ended before it started: %v", err)
			}

			if err := c.StopProcess(ctx, "tree", grace); err != nil {
				t.Fatalf("stop the process: %v", err)
			}
			if killed := awaitProcess(t, c, "tree", models.ProcessKilled); killed.Exit == nil || killed.Exit.Signal != int(sig) {
				t.Fatalf("tree ended as %+v, want killed by signal %d", killed, sig)
			}
			waitFor(t, 10*time.Second, fmt.Sprintf("the sleep the process forked, pid %d, to end", forked), func() bool { return gone(forked) })

			if err := proc.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the guest ended with its process: %v", err)
			}
			if err := syscall.Kill(execPID, 0); err != nil {
				t.Fatalf("the exec session's pid %d ended with the process: %v", execPID, err)
			}
			select {
			case err := <-execDone:
				t.Fatalf("the exec session ended with %v, want it attached", err)
			default:
			}
		})
	}
}

// The stop answers once its process is reaped, on its own goroutine, so a run behind it answers while it waits out the grace.
func TestTransportStopProcessAnswersAfterTheReapWhileOtherRequestsGoOn(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	if err := c.Run(ctx, named("stubborn", "ignoreterm")); err != nil {
		t.Fatalf("run: %v", err)
	}
	logs, held := openLogs(ctx, t, dial, "stubborn")
	sendOffset(t, logs, held[0])
	if out := readOutput(t, logs, len("ready\n")); out != "ready\n" {
		t.Fatalf("stubborn wrote %q, want ready once it ignores TERM", out)
	}
	// Acked, so the kill's report waits on no output.
	sendOffset(t, logs, held[0]+uint64(len("ready\n")))

	stopped := make(chan error, 1)
	go func() { stopped <- c.StopProcess(ctx, "stubborn", 2*time.Second) }()
	time.Sleep(200 * time.Millisecond)
	asked := time.Now()
	if err := c.Run(ctx, named("other", "exit:0")); err != nil {
		t.Fatalf("a run during the stop: %v", err)
	}
	if waited := time.Since(asked); waited > time.Second {
		t.Fatalf("the run answered after %s, behind the stop's grace", waited)
	}
	select {
	case err := <-stopped:
		t.Fatalf("the stop answered %v before its grace ran out", err)
	default:
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop the process: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stop never answered")
	}
	if killed := awaitProcess(t, c, "stubborn", models.ProcessKilled); killed.Exit == nil || killed.Exit.Signal != int(syscall.SIGKILL) {
		t.Fatalf("stubborn ended as %+v, want killed by the grace's SIGKILL", killed)
	}
}

// A process that ends leaves nothing in its group, even a child that ignored the TERM, so no verb has to reach it after the end.
func TestTransportAnEndedProcessLeavesNothingInItsGroup(t *testing.T) {
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
			c := connectReady(ctx, t, dial)
			pidFile := filepath.Join(shortDir(t), "forked")
			if err := c.Run(ctx, supervisor.RunSpec{Name: "tree", Argv: tc.argv(pidFile), Env: os.Environ()}); err != nil {
				t.Fatalf("run: %v", err)
			}
			forked := forkedPIDs(t, pidFile, 1)[0]
			end := endShown(c, "tree")

			if tc.stop {
				if err := c.StopProcess(ctx, "tree", 10*time.Second); err != nil {
					t.Fatalf("stop the process: %v", err)
				}
			}
			select {
			case <-end:
			case <-time.After(10 * time.Second):
				t.Fatal("the process never ended")
			}
			waitFor(t, 10*time.Second, fmt.Sprintf("the child the process left, pid %d, to end", forked), func() bool { return gone(forked) })
		})
	}
}

// A start again never runs beside what the last run left behind, so a restart kills the last run's group.
func TestTransportARestartEndsWhatTheLastRunForked(t *testing.T) {
	_, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	pidFile := filepath.Join(shortDir(t), "forked")
	spec := treeSpec("tree", pidFile, "exit 1")
	spec.Restart, spec.Retries, spec.Backoff = models.RestartOnFailure, 1, time.Millisecond
	if err := c.Run(ctx, spec); err != nil {
		t.Fatalf("run: %v", err)
	}
	forked := forkedPIDs(t, pidFile, 2)

	waitFor(t, 10*time.Second, fmt.Sprintf("the first run's sleep, pid %d, to end", forked[0]), func() bool { return gone(forked[0]) })
}

// A kill of a guest whose processes already ended ends nothing, so a host lost before the cut must read the freeze off the replay and thaw it (SHARD-344).
func TestTransportKillReplaysFrozenOnTheNextHost(t *testing.T) {
	proc, dial := startTransport(t)
	ctx := testContext(t)
	c := connectReady(ctx, t, dial)
	if err := c.Run(ctx, named("once", "exit:0")); err != nil {
		t.Fatalf("run: %v", err)
	}
	awaitProcess(t, c, "once", models.ProcessExited)
	if err := c.Kill(ctx); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("hang up: %v", err)
	}
	// Past a reap tick, which must not end a guest the kill found nothing to forward to.
	time.Sleep(reapEvery + 500*time.Millisecond)

	next := connect(ctx, t, dial)
	if state := awaitKind(t, next, supervisor.KindState); !state.Frozen {
		t.Fatal("the replay after a kill says not frozen, so the next host would strand it")
	}

	// A stop still ends the guest the kill left frozen.
	if err := next.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitCleanExit(ctx, t, proc, "the stop")
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

// idleGuest is a guest no supervise runs, for a test that drives the transport alone; its signals go back to the test binary after.
func idleGuest(t *testing.T, r reporter) *guest {
	t.Helper()

	g := newGuest(r)
	t.Cleanup(func() {
		signal.Stop(g.childDeaths)
		signal.Stop(g.stopSignals)
	})

	return g
}

// A guest with nothing to forward a stop to goes at once, so the host must hold its answer first (SHARD-483).
func TestTransportAnswersAStopBeforeTheGuestActsOnIt(t *testing.T) {
	ctx := testContext(t)
	host, conn := net.Pipe()
	closeLater(t, host)
	guest := &writeWatch{Conn: conn, writing: make(chan struct{}, 1)}
	tr := &transport{attached: make(chan struct{}, 1), control: guest}
	tr.g = idleGuest(t, tr)
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
	proc, dial := startTransport(t)
	ctx := testContext(t)
	attach := func(wantFrozen bool) *supervisor.Control {
		t.Helper()
		c := connect(ctx, t, dial)
		if state := awaitKind(t, c, supervisor.KindState); state.Frozen != wantFrozen {
			t.Fatalf("the replay says frozen %t, want %t", state.Frozen, wantFrozen)
		}

		return c
	}

	c := attach(false)
	for range 2 {
		if err := c.Freeze(ctx, models.VerbPause); err != nil {
			t.Fatalf("freeze: %v", err)
		}
	}
	c = attach(true)
	for range 2 {
		if err := c.Thaw(ctx); err != nil {
			t.Fatalf("thaw: %v", err)
		}
	}
	c = attach(false)

	// A stop still ends a frozen guest.
	if err := c.Run(ctx, named("web", "sleep:60000")); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := c.Freeze(ctx, models.VerbPause); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitCleanExit(ctx, t, proc, "the stop")
}

func TestTransportExecWithNoStdinSeesEOF(t *testing.T) {
	_, dial := startTransport(t)

	// The echo role copies stdin until EOF; with no stdin it must still end, and promptly.
	execCtx, cancel := context.WithTimeout(testContext(t), 5*time.Second)
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
	connect(ctx, t, dial)

	for i := range 40 {
		_, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"/nonexistent/cmd"}}, models.ExecSpec{})
		if _, ok := errors.AsType[*models.CommandNotStartedError](err); !ok {
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
	closeLater(t, oldHost)
	closeLater(t, newHost)
	tr := &transport{control: newGuest}

	// The old host asked, the new one replaced it; the old answer must reach neither.
	tr.answer(oldGuest, 1, nil)
	if err := newHost.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var stray supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(newHost), &stray); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the new host read %+v (%v), want nothing", stray, err)
	}

	go tr.answer(newGuest, 2, nil)
	if err := newHost.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reply supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(newHost), &reply); err != nil || reply.ID != 2 || reply.Kind != supervisor.KindDone {
		t.Fatalf("the new host read %+v (%v), want done 2", reply, err)
	}
}

// A freeze whose host was replaced before the answer is undone, since the new host's replay may have read the root before it froze.
func TestAFreezeNoHostHeardIsUndone(t *testing.T) {
	_, oldGuest := net.Pipe()
	newHost, nextGuest := net.Pipe()
	closeLater(t, oldGuest)
	closeLater(t, newHost)
	tr := &transport{control: nextGuest}
	tr.g = idleGuest(t, tr)
	serveCommands(t, tr.g)

	tr.freeze(oldGuest, 1, models.VerbFork)
	if tr.g.frozen.Load() != nil {
		t.Fatal("the root stays frozen after a freeze no host heard")
	}

	go tr.freeze(nextGuest, 2, models.VerbFork)
	if err := newHost.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
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
	c := connect(ctx, t, dial)
	if err := c.Freeze(ctx, models.VerbFork); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	_, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: childArgv("exit:0")}, models.ExecSpec{})
	if _, ok := errors.AsType[*models.CommandNotStartedError](err); !ok || !strings.Contains(err.Error(), "sandbox sb could not run the command: a fork holds the sandbox frozen") {
		t.Fatalf("exec while frozen gave %v, want the refusal that names the fork", err)
	}

	if err := c.Thaw(ctx); err != nil {
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
	c := connectReady(ctx, t, dial)
	spec := named("flaky", "sleep:300")
	spec.Restart, spec.Backoff = models.RestartAlways, 10*time.Millisecond
	if err := c.Run(ctx, spec); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Off a VM nothing really freezes, so the process exits under the freeze and its restart falls due there.
	if err := c.Freeze(ctx, models.VerbFork); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	awaitProcess(t, c, "flaky", models.ProcessRestarting)
	time.Sleep(200 * time.Millisecond)
	thawed := time.Now()
	if err := c.Thaw(ctx); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	running := awaitProcess(t, c, "flaky", models.ProcessRunning)
	if running.Restarts != 1 || running.StartedAt.Before(thawed) {
		t.Fatalf("the restart reported %+v, want restart 1 started after the thaw at %s", running, thawed.UTC())
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
	if err := c.Close(); err != nil {
		t.Fatalf("hang up: %v", err)
	}
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
	closeLater(t, c)
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
	tr.g = idleGuest(t, tr)

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
