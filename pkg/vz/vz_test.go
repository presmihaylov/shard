package vz

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

func TestAZeroMemoryRequestIsTheDefaultAndNotTheFrameworksMinimum(t *testing.T) {
	if got := Memory(0); got != DefaultMemory || DefaultMemory != 512<<20 {
		t.Fatalf("Memory(0) = %d, want 512 MiB", got)
	}
	if got := Memory(1 << 20); got != 1<<20 {
		t.Fatalf("Memory(1 MiB) = %d", got)
	}
	if err := CheckMemory(0, Range{Min: 4 << 20, Max: 1 << 40}); err != nil {
		t.Fatalf("CheckMemory(0) = %v, want the default to pass", err)
	}
	err := CheckMemory(0, Range{Min: 4 << 20, Max: 256 << 20})
	if err == nil || !strings.Contains(err.Error(), "536870912 bytes") {
		t.Fatalf("CheckMemory(0) = %v, want the default refused by name", err)
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
	if _, _, err := Adopt(socket); err != nil {
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
	_, err = client.State()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("State() = %v, want the deadline", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("the deadline did not bound the call")
	}
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
	if _, _, err := Adopt(socket); err != nil {
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
	if info, err := client.Stop(); err != nil || info.State != StateStopped {
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
	if _, err := client.Connect(9); err == nil || !strings.Contains(err.Error(), "nothing listens there") {
		t.Fatalf("Connect(9) = %v", err)
	}
}

func TestConnectSplicesTheGuestStreamOntoTheSocket(t *testing.T) {
	machine := &fake{state: StateRunning}
	client := serve(t, machine)

	conn, err := client.Connect(5000)
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
	_, _, err := Adopt(filepath.Join(t.TempDir(), "gone.sock"))
	if err == nil || !strings.Contains(err.Error(), "adopt the shim") {
		t.Fatalf("Adopt() = %v", err)
	}
}
