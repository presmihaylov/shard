package vz

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/pidpin/pidpintest"
)

func TestSaveRestoreFailsClosedOnEveryRowOfTheMatrix(t *testing.T) {
	cases := []struct {
		name                  string
		goos, goarch, version string
		want                  bool
	}{
		{"macOS 14 on arm64", "darwin", "arm64", "14.0", true},
		{"macOS 26 on arm64", "darwin", "arm64", "26.6", true},
		{"a patch version", "darwin", "arm64", "15.3.1", true},
		{"linux", "linux", "arm64", "14.0", false},
		{"an Intel Mac", "darwin", "amd64", "14.0", false},
		{"macOS 13", "darwin", "arm64", "13.6.7", false},
		{"an empty version", "darwin", "arm64", "", false},
		{"a malformed version", "darwin", "arm64", "Sequoia", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SaveRestoreSupported(c.goos, c.goarch, c.version); got != c.want {
				t.Fatalf("SaveRestoreSupported(%q, %q, %q) = %v, want %v", c.goos, c.goarch, c.version, got, c.want)
			}
		})
	}
}

func TestTheCPUDefaultIsTheHostCountHeldInsideTheRange(t *testing.T) {
	allowed := Range{Min: 1, Max: 8}
	for host, want := range map[int]uint{4: 4, 8: 8, 12: 8, 0: 1, -1: 1} {
		if got := DefaultCPUs(host, allowed); got != want {
			t.Fatalf("DefaultCPUs(%d) = %d, want %d", host, got, want)
		}
	}
}

func TestAnExplicitValueOutsideTheRangeIsRefusedAndNamesIt(t *testing.T) {
	allowed := Range{Min: 128, Max: 4096}
	for _, ok := range []uint64{128, 4096, 1024} {
		if err := CheckMemory(ok, allowed); err != nil {
			t.Fatalf("CheckMemory(%d) = %v, want nil", ok, err)
		}
		if err := CheckCPUs(uint(ok), allowed); err != nil {
			t.Fatalf("CheckCPUs(%d) = %v, want nil", ok, err)
		}
	}
	if err := CheckCPUs(0, allowed); err != nil {
		t.Fatalf("CheckCPUs(0) = %v, want nil", err)
	}
	for _, bad := range []uint64{127, 4097, 1} {
		err := CheckMemory(bad, allowed)
		if err == nil || !strings.Contains(err.Error(), "128 to 4096") {
			t.Fatalf("CheckMemory(%d) = %v, want the range named", bad, err)
		}
		err = CheckCPUs(uint(bad), allowed)
		if err == nil || !strings.Contains(err.Error(), "128 to 4096") {
			t.Fatalf("CheckCPUs(%d) = %v, want the range named", bad, err)
		}
	}
}

func TestAZeroMemoryRequestHasNoDefaultAndIsRefused(t *testing.T) {
	err := CheckMemory(0, Range{Min: 4 << 20, Max: 1 << 40})
	if err == nil || !strings.Contains(err.Error(), "0 bytes") {
		t.Fatalf("CheckMemory(0) = %v, want zero refused by name", err)
	}
}

func TestListenRefusesALiveShimAndReplacesADeadOne(t *testing.T) {
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(listener, &fake{state: StateRunning}, log.New(io.Discard, "", 0)) }()

	if _, err := Listen(socket); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("Listen over a live shim = %v, want ErrSocketInUse", err)
	}
	if _, _, err := Adopt(t.Context(), socket); err != nil {
		t.Fatalf("the first shim is no longer answering: %v", err)
	}

	// A dead shim leaves its path bound to nothing; that one is ours to take.
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := errors.Join(listener.Close(), <-done); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the dead shim's path is gone: %v", err)
	}
	again, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen over a dead shim = %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAShimThatAcceptsAndNeverAnswersIsGivenUpOn(t *testing.T) {
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-t.Context().Done()
		}
	}()

	client := &Client{socket: socket, timeout: 200 * time.Millisecond}
	started := time.Now()
	_, err = client.State(t.Context())
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("State() = %v, want the deadline", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("the deadline did not bound the call")
	}
}

// SHARD-349: a stop must end on its own clock, so the caller's earlier deadline wins over callTimeout.
func TestTheCallersDeadlineBoundsACallToAShimThatNeverAnswers(t *testing.T) {
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = (&Client{socket: socket}).Stop(ctx)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Stop() = %v, want the deadline", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("Stop took %s, want the caller's 200ms", took)
	}
}

// An exec holds admit through its dial, so a Connect its caller gave up on must not keep a fork waiting.
func TestACancelEndsAConnectTheShimAcceptedAndNeverAnswered(t *testing.T) {
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	asked := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var first [1]byte
		if _, err := conn.Read(first[:]); err == nil {
			close(asked)
		}
		<-t.Context().Done()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := (&Client{socket: socket}).Connect(ctx, 5000)
		if err == nil {
			err = errors.Join(errors.New("Connect opened a stream"), conn.Close())
		}
		done <- err
	}()
	<-asked
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Connect() = %v, want the cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connect still waits on the shim a second after its caller cancelled")
	}
}

// frozenShimEnv names the socket a re-run of this test binary listens on and never accepts, as a shim stopped by SIGSTOP would.
const frozenShimEnv = "VZ_TEST_FROZEN_SHIM"

func TestFrozenShimHelper(t *testing.T) {
	socket := os.Getenv(frozenShimEnv)
	if socket == "" {
		t.Skip("a helper process for TestKillEndsAShimTooFrozenToAnswer")
	}
	listener, err := net.Listen("unix", socket+".bind")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// The file exists from the bind, before the listen; a dial in that gap is refused, so the test only sees the path once it listens.
	if err := os.Rename(socket+".bind", socket); err != nil {
		t.Fatal(err)
	}
	select {}
}

// dialed reports whether the socket takes a dial: the file exists at bind(), before listen(), so only a dial proves the shim listens.
func dialed(t *testing.T, socket string) bool {
	t.Helper()
	probe, err := net.Dial("unix", socket)
	if err != nil {
		return false
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	return true
}

// SHARD-349: a frozen shim takes the dial and never the call, so the kill names it by the kernel's peer pid and never waits for an answer.
func TestKillEndsAShimTooFrozenToAnswer(t *testing.T) {
	pidpintest.Require(t)
	socket, cmd, exited := listening(t)
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}

	client := &Client{socket: socket}
	started := time.Now()
	if err := client.Kill(); err != nil {
		t.Fatalf("Kill of a frozen shim = %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the frozen shim still runs 5s after Kill")
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("Kill took %s, want it done at the dial", took)
	}
	if err := client.Kill(); !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
		t.Errorf("Kill of an ended shim = %v, want the socket refused", err)
	}
}

// A pin on a pid the shim socket does not name again after the pin sends no signal, and says so, so the caller kills by its own record instead.
func TestKillThroughAPinTheSocketDoesNotProveSignalsNobody(t *testing.T) {
	pidpintest.Require(t)
	socket, _, exited := listening(t)
	innocent := exec.Command("sleep", "60")
	if err := innocent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := innocent.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := innocent.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})

	client := &Client{socket: socket}
	if err := client.killPinned(innocent.Process.Pid); !errors.Is(err, ErrUnproven) {
		t.Fatalf("killPinned of a pid the socket does not name = %v, want ErrUnproven", err)
	}
	if err := syscall.Kill(innocent.Process.Pid, 0); err != nil {
		t.Fatalf("the pinned process the socket does not name was hit: %v", err)
	}
	select {
	case err := <-exited:
		t.Fatalf("the shim the socket names was hit: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// listening runs a helper that listens on a socket and never accepts, in a group of its own, and is killed at cleanup.
func listening(t *testing.T) (string, *exec.Cmd, <-chan error) {
	t.Helper()
	socket := filepath.Join(shortRoot(t), "shim.sock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFrozenShimHelper$")
	cmd.Env = append(os.Environ(), frozenShimEnv+"="+socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})
	for !dialed(t, socket) {
		select {
		case err := <-exited:
			t.Fatalf("the helper exited before it listened: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	return socket, cmd, exited
}

func TestAnIdleConnectionDoesNotKeepServeFromReturning(t *testing.T) {
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(listener, &fake{state: StateRunning}, log.New(io.Discard, "", 0)) }()

	idle, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if _, _, err := Adopt(t.Context(), socket); err != nil {
		t.Fatal(err)
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(handshakeTimeout):
		t.Fatal("Serve waited on a connection that never sent a frame")
	}
	if _, err := idle.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection was not closed: %v", err)
	}
}

func TestAFailedHandshakeLeavesNothingForShutdownToClose(t *testing.T) {
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var logs safeBuffer
	done := make(chan error, 1)
	go func() { done <- Serve(listener, &fake{state: StateRunning}, log.New(&logs, "", 0)) }()

	const clients = 8
	for range clients {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Each dropped handshake is logged once, and the count is how the test knows every handler has left.
	for deadline := time.Now().Add(handshakeTimeout); logs.count("shim socket: read the frame length") < clients; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d dropped handshakes were logged", logs.count("shim socket: read the frame length"), clients)
		}
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := logs.count("pending"); n != 0 {
		t.Fatalf("shutdown closed %d connections whose handshake had already failed:\n%s", n, logs.String())
	}
}

// safeBuffer is a log sink the test reads while Serve still writes it.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

func (s *safeBuffer) count(text string) int {
	return strings.Count(s.String(), text)
}

// fake stands in for the framework VM: it records the verbs and serves a guest stream from a pipe.
type fake struct {
	state State
	verbs []string
	saved string
	guest net.Conn
	// frames is the host end of a datagram pair the network verb hands out; nil is a VM with no network.
	frames *os.File
}

func (f *fake) State() State      { return f.state }
func (f *fake) MachineID() string { return "id-1" }
func (f *fake) Pause() error      { f.verbs = append(f.verbs, "pause"); f.state = StatePaused; return nil }
func (f *fake) Resume() error {
	f.verbs = append(f.verbs, "resume")
	f.state = StateRunning
	return nil
}
func (f *fake) Stop() error { f.verbs = append(f.verbs, "stop"); f.state = StateStopped; return nil }
func (f *fake) Save(path string) error {
	if f.state != StatePaused {
		return errors.New("save needs a paused vm")
	}
	f.saved = path

	return nil
}
func (f *fake) Network() (*os.File, error) {
	if f.frames == nil {
		return nil, errors.New("the vm has no network device")
	}

	return f.frames, nil
}
func (f *fake) Connect(port uint32) (net.Conn, error) {
	if port != 5000 {
		return nil, errors.New("nothing listens there")
	}
	host, guest := net.Pipe()
	f.guest = guest

	return host, nil
}

// shortRoot skips t.TempDir, whose path carries the test name past the 104 bytes a macOS socket path allows.
func shortRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "shard") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})

	return root
}

func serve(t *testing.T, machine Machine) *Client {
	t.Helper()
	socket := filepath.Join(shortRoot(t), "shim.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(listener, machine, log.New(io.Discard, "", 0)) }()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	return &Client{socket: socket}
}

func TestTheVerbsReachTheMachineAndTheReplyCarriesItsState(t *testing.T) {
	machine := &fake{state: StateRunning}
	client := serve(t, machine)

	info, err := client.Pause()
	if err != nil || info.State != StatePaused || info.MachineID != "id-1" || info.PID == 0 {
		t.Fatalf("Pause() = %+v, %v", info, err)
	}
	if _, err := client.Save("/tmp/vm.vzvmstate"); err != nil || machine.saved != "/tmp/vm.vzvmstate" {
		t.Fatalf("Save() = %v, saved %q", err, machine.saved)
	}
	if info, err := client.Resume(); err != nil || info.State != StateRunning {
		t.Fatalf("Resume() = %+v, %v", info, err)
	}
	if info, err := client.Stop(t.Context()); err != nil || info.State != StateStopped {
		t.Fatalf("Stop() = %+v, %v", info, err)
	}
	if got := strings.Join(machine.verbs, " "); got != "pause resume stop" {
		t.Fatalf("verbs = %q", got)
	}
}

func TestAMachineErrorComesBackAsTheVerbsError(t *testing.T) {
	client := serve(t, &fake{state: StateRunning})

	_, err := client.Save("/tmp/x")
	if err == nil || !strings.Contains(err.Error(), "save: save needs a paused vm") {
		t.Fatalf("Save() on a running vm = %v", err)
	}
	if _, err := client.Connect(t.Context(), 9); err == nil || !strings.Contains(err.Error(), "nothing listens there") {
		t.Fatalf("Connect(9) = %v", err)
	}
}

func TestConnectSplicesTheGuestStreamOntoTheSocket(t *testing.T) {
	machine := &fake{state: StateRunning}
	client := serve(t, machine)

	conn, err := client.Connect(t.Context(), 5000)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		line, _ := bufio.NewReader(machine.guest).ReadString('\n')
		_, _ = machine.guest.Write([]byte("echo " + line))
		machine.guest.Close()
	}()
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "echo hello\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// The network verb hands the daemon the very socket the VM writes to: a datagram in on one end is a datagram out on the other.
func TestNetworkHandsTheFramesSocketOverByDescriptor(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	guest := os.NewFile(uintptr(fds[0]), "guest")
	defer guest.Close()
	machine := &fake{state: StateRunning, frames: os.NewFile(uintptr(fds[1]), "host")}
	// Registered before serve, so the shim's copy closes after the shim has stopped, as in the real shim.
	t.Cleanup(func() { machine.frames.Close() })
	client := serve(t, machine)

	frames, err := client.Network()
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Close()

	if _, err := guest.Write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := frames.Read(buf)
	if err != nil || string(buf[:n]) != "frame" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	if _, err := frames.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	n, err = guest.Read(buf)
	if err != nil || string(buf[:n]) != "reply" {
		t.Fatalf("read back %q, %v", buf[:n], err)
	}
}

func TestNetworkRefusesAVMWithoutOne(t *testing.T) {
	client := serve(t, &fake{state: StateRunning})
	if _, err := client.Network(); err == nil || !strings.Contains(err.Error(), "no network device") {
		t.Fatalf("Network() = %v", err)
	}
}

func TestAdoptRefusesASocketNobodyAnswers(t *testing.T) {
	_, _, err := Adopt(t.Context(), filepath.Join(t.TempDir(), "gone.sock"))
	if err == nil || !strings.Contains(err.Error(), "adopt the shim") {
		t.Fatalf("Adopt() = %v", err)
	}
}
