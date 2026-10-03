package vzvm_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/supervisor"
)

// The test binary plays the shim when the provider execs it with this set; the guest is the real shard-init over unix sockets.
const (
	fakeShimEnv = "VZVM_FAKE_SHIM"
	fakeInitEnv = "VZVM_FAKE_INIT"
	fakeRunEnv  = "VZVM_FAKE_RUN"
	// impostorRole runs the binary with a shim's arguments, serving nothing, as a process that only claims a socket would.
	impostorRole = "impostor"
)

// initBinary is the shard-init the fake shim runs in place of a VM, built once per test run unless the env names one.
var initBinary string

// guardHost is the integration suite's hold on the host for the run, and its release; a plain test run leaves it nil.
var guardHost func() (release func() error, err error)

func TestMain(m *testing.M) {
	if os.Getenv(fakeShimEnv) == impostorRole {
		time.Sleep(time.Hour)

		return
	}
	// The shim passes its whole environment to the guest, so only the -transport argv says which one this is.
	if os.Getenv(bootFailingGuestEnv) == "1" && len(os.Args) == 3 && os.Args[1] == "-transport" {
		if err := bootFailingGuest(strings.TrimPrefix(os.Args[2], "unix:")); err != nil {
			fmt.Fprintln(os.Stderr, "boot failing guest:", err)
			os.Exit(1)
		}
		os.Exit(models.SupervisorFailedExitCode)
	}
	if os.Getenv(fakeShimEnv) == "1" {
		if err := fakeShim(); err != nil {
			fmt.Fprintln(os.Stderr, "fake shim:", err)
			os.Exit(1)
		}

		return
	}

	os.Exit(runTests(m))
}

func runTests(m *testing.M) (code int) {
	if guardHost != nil {
		release, err := guardHost()
		if err != nil {
			fmt.Fprintln(os.Stderr, "vzvm tests:", err)

			return 1
		}
		defer func() {
			if err := release(); err != nil {
				fmt.Fprintln(os.Stderr, "give the host back:", err)

				code = 1
			}
		}()
	}

	initBinary = os.Getenv(fakeInitEnv)
	if initBinary == "" {
		dir, err := os.MkdirTemp("", "vzinit")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)

			return 1
		}
		defer os.RemoveAll(dir)
		initBinary = filepath.Join(dir, "shard-init")
		build := exec.Command("go", "build", "-o", initBinary, "../../../cmd/shard-init")
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build shard-init:", err)

			return 1
		}
	}
	// A shim runs in its own group, which a timeout or a kill of this run never reaches: it inherits the read end and goes at EOF.
	var run [2]int
	if err := syscall.Pipe(run[:]); err != nil {
		fmt.Fprintln(os.Stderr, "make the run pipe:", err)

		return 1
	}
	syscall.CloseOnExec(run[1])
	// Every shim the provider starts from here is this binary, and inherits the switch.
	os.Setenv(fakeShimEnv, "1")
	os.Setenv(fakeInitEnv, initBinary)
	os.Setenv(fakeRunEnv, strconv.Itoa(run[0]))

	return m.Run()
}

// fakeShim is cmd/shard-vz-shim without the framework: one shard-init process behind the shim socket.
func fakeShim() error {
	flags := flag.NewFlagSet("fake-shim", flag.ContinueOnError)
	encoded := flags.String("config", "", "")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	var cfg vz.Config
	if err := json.Unmarshal([]byte(*encoded), &cfg); err != nil {
		return fmt.Errorf("decode -config: %w", err)
	}
	fd, err := strconv.Atoi(os.Getenv(fakeRunEnv))
	if err != nil {
		return fmt.Errorf("read %s: %w", fakeRunEnv, err)
	}
	// The guest has no use for the run's pipe.
	syscall.CloseOnExec(fd)
	run := os.NewFile(uintptr(fd), "run")

	listener, err := vz.Listen(cfg.Socket)
	if err != nil {
		return err
	}
	listener = countingListener{Listener: listener, path: filepath.Join(filepath.Dir(cfg.Socket), acceptsFile)}
	machine, err := bootFake(cfg)
	if err != nil {
		return errors.Join(err, listener.Close())
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	served := make(chan error, 1)
	go func() { served <- vz.Serve(listener, machine, logger) }()
	runEnded := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, run)
		runEnded <- err
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	// SIGUSR1 is a vsock reset, as a sleep of the host can cause: every open stream ends, the guest goes on.
	resets := make(chan os.Signal, 1)
	signal.Notify(resets, syscall.SIGUSR1)
	// SIGUSR2 is the same reset on a transport that takes a while to settle: a dial answers, and ends at once.
	holds := make(chan os.Signal, 1)
	signal.Notify(holds, syscall.SIGUSR2)
	for {
		select {
		case <-resets:
			machine.dropStreams()
		case <-holds:
			machine.mu.Lock()
			machine.holdUntil = time.Now().Add(resetHold)
			machine.mu.Unlock()
			machine.dropStreams()
		case <-signals:
			return stopFake(machine, listener, served)
		case err := <-runEnded:
			// No test is left to stop this sandbox, so the shim does.
			return errors.Join(err, stopFake(machine, listener, served))
		case <-machine.exited:
			return errors.Join(listener.Close(), <-served)
		}
	}
}

// stopFake ends the guest, then the socket.
func stopFake(machine *fakeMachine, listener net.Listener, served <-chan error) error {
	if err := machine.Stop(); err != nil {
		return err
	}
	<-machine.exited

	return errors.Join(listener.Close(), <-served)
}

// fakeMachine is a shard-init process in its own group: a pause is SIGSTOP, a save a marker file, a stop SIGKILL.
type fakeMachine struct {
	id     string
	dir    string
	cmd    *exec.Cmd
	exited chan struct{}

	mu      sync.Mutex
	state   vz.State
	streams map[net.Conn]struct{}
	// holdUntil is how long a dial answers with a stream that ends at once, after a reset SIGUSR2 asked for.
	holdUntil time.Time
	// frozen is the guest root as the host last froze or thawed it; the fake's guest is not PID 1 and freezes nothing itself.
	frozen bool
	// controls counts the control streams the host opened, so a reset can wait for the next one.
	controls int
}

// unfrozenFile lands in the state directory when a pause stopped a guest whose root still took writes, so a clone could read a torn disk.
const unfrozenFile = "unfrozen-pause"

// frozenFile is in the state directory while the guest's root is frozen, so a test sees a guest left unable to write.
const frozenFile = "frozen-root"

// cutFreezeFile in the state directory lets the next freeze reach the guest and loses its answer, as a reset between the two would.
const cutFreezeFile = "cut-freeze-answer"

// resetOnPauseFile in the state directory resets every stream under the next VM pause, and holds that pause until the host dialed again.
const resetOnPauseFile = "reset-on-pause"

// holdDialsFile in the state directory answers every dial with a stream that ends at once, until the test removes it.
const holdDialsFile = "hold-dials"

// refuseResumeFile in the state directory fails every resume of the VM, until the test removes it.
const refuseResumeFile = "refuse-resume"

// orderFile in the state directory, once a test creates it, takes one line per freeze, reseed and thaw in the order the guest reads them.
const orderFile = "control-order"

// controlsFile in the state directory, once a test creates it, takes one line per control stream the host opens, so a test counts the attaches.
const controlsFile = "controls"

// acceptsFile in the state directory, once a test creates it, takes one line per connection the shim accepts, so a test reads what a frozen shim's socket queue held.
const acceptsFile = "accepts"

// floodFile in the state directory floods the next control stream past its state line, as guest root writing to PID 1's control fd would.
const floodFile = "flood-control"

// floodEveryFile in the state directory floods every control stream past its state line, for as long as it stays there.
const floodEveryFile = "flood-every-control"

// dialsFile in the state directory, once a test creates it, takes one line per control stream the host dials.
const dialsFile = "control-dials"

// resetHold is longer than the entrypoint the hold test runs, so its exit lands while no stream is open.
const resetHold = 1500 * time.Millisecond

func bootFake(cfg vz.Config) (*fakeMachine, error) {
	id := cfg.MachineID
	if id == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		id = hex.EncodeToString(b[:])
	}
	// The framework refuses a save made under another identifier, and so does the fake.
	restoredFrozen := false
	if cfg.Restore != "" {
		saved, err := os.ReadFile(cfg.Restore)
		if err != nil {
			return nil, fmt.Errorf("read the saved state: %w", err)
		}
		savedID, frozen := strings.CutSuffix(strings.TrimSpace(string(saved)), "\n"+frozenFile)
		if savedID != id {
			return nil, fmt.Errorf("the saved state belongs to machine %s, not %s", savedID, id)
		}
		restoredFrozen = frozen
	}

	dir := filepath.Join(filepath.Dir(cfg.Socket), "guest")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	console, err := os.OpenFile(cfg.Console, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer console.Close()

	cmd := exec.Command(os.Getenv(fakeInitEnv), "-transport", "unix:"+dir)
	cmd.Stdout = console
	cmd.Stderr = console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shard-init: %w", err)
	}

	m := &fakeMachine{id: id, dir: dir, cmd: cmd, exited: make(chan struct{}), state: vz.StateRunning, streams: map[net.Conn]struct{}{}}
	go func() {
		defer close(m.exited)
		// The exit is the guest powering off; the group may still hold an entrypoint that ignored TERM.
		_ = cmd.Wait()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		m.mu.Lock()
		m.state = vz.StateStopped
		m.mu.Unlock()
	}()
	// A save holds the guest's memory with its root frozen; the fake's guest is a fresh process, so it is frozen again.
	if restoredFrozen {
		if err := errors.Join(freezeGuest(dir), m.setFrozen(true)); err != nil {
			return nil, errors.Join(err, m.Stop())
		}
	}

	return m, nil
}

// freezeGuest asks the guest to freeze over a control connection of its own, before any host attaches.
func freezeGuest(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	control, err := supervisor.Connect(ctx, func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	})
	if err != nil {
		return err
	}
	if _, err := control.Next(); err != nil {
		return errors.Join(err, control.Close())
	}

	return errors.Join(control.Freeze(ctx), control.Close())
}

// setFrozen tracks the guest's root, and marks it frozen in the state directory for a test to see.
func (m *fakeMachine) setFrozen(frozen bool) error {
	m.mu.Lock()
	m.frozen = frozen
	m.mu.Unlock()
	marker := filepath.Join(filepath.Dir(m.dir), frozenFile)
	if frozen {
		return os.WriteFile(marker, nil, 0o600)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

type countingListener struct {
	net.Listener
	path string
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return conn, nil
	}
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	_, err = f.WriteString("accept\n")
	if err := errors.Join(err, f.Close()); err != nil {
		return nil, errors.Join(err, conn.Close())
	}

	return conn, nil
}

func (m *fakeMachine) State() vz.State {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state
}

func (m *fakeMachine) MachineID() string { return m.id }

// take removes a marker the test left in the state directory, and says whether it was there.
func (m *fakeMachine) take(name string) (bool, error) {
	err := os.Remove(filepath.Join(filepath.Dir(m.dir), name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

// has says whether the test left a marker in the state directory.
func (m *fakeMachine) has(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(filepath.Dir(m.dir), name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

// note appends kind to the order file, when the test made one.
func (m *fakeMachine) note(kind string) error { return m.appendTo(orderFile, kind) }

// appendTo appends one line to a file in the state directory, when the test made one.
func (m *fakeMachine) appendTo(name, line string) error {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(m.dir), name), os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")

	return errors.Join(err, f.Close())
}

func (m *fakeMachine) Pause() error {
	reset, err := m.take(resetOnPauseFile)
	if err != nil {
		return err
	}
	if reset {
		if err := m.resetAndAwaitHost(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	frozen := m.frozen
	m.mu.Unlock()
	if !frozen {
		if err := os.WriteFile(filepath.Join(filepath.Dir(m.dir), unfrozenFile), nil, 0o600); err != nil {
			return err
		}
	}

	return m.move(vz.StateRunning, vz.StatePaused, syscall.SIGSTOP)
}

// resetAndAwaitHost drops every stream and returns once the host opened a control stream again and had time to act on its replay.
func (m *fakeMachine) resetAndAwaitHost() error {
	m.mu.Lock()
	before := m.controls
	m.mu.Unlock()
	m.dropStreams()

	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.Lock()
		dialed := m.controls > before
		m.mu.Unlock()
		if dialed {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the host did not open a control stream again after the reset")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A thaw the host sends on its replay lands well inside this, and the pause then sees the root it left.
	time.Sleep(500 * time.Millisecond)

	return nil
}

func (m *fakeMachine) Resume() error {
	refused, err := m.has(refuseResumeFile)
	if err != nil {
		return err
	}
	if refused {
		return errors.New("the vm refuses to resume")
	}

	return m.move(vz.StatePaused, vz.StateRunning, syscall.SIGCONT)
}

func (m *fakeMachine) move(from, to vz.State, sig syscall.Signal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != from {
		return fmt.Errorf("the vm is %s, not %s", m.state, from)
	}
	if err := syscall.Kill(-m.cmd.Process.Pid, sig); err != nil {
		return err
	}
	m.state = to

	return nil
}

func (m *fakeMachine) Save(path string) error {
	m.mu.Lock()
	state, frozen := m.state, m.frozen
	m.mu.Unlock()
	if state != vz.StatePaused {
		return fmt.Errorf("the vm is %s, and only a paused one saves", state)
	}
	saved := m.id
	if frozen {
		saved += "\n" + frozenFile
	}

	return os.WriteFile(path, []byte(saved), 0o600)
}

func (m *fakeMachine) Stop() error {
	if err := syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

func (m *fakeMachine) Connect(port uint32) (net.Conn, error) {
	conn, err := net.Dial("unix", filepath.Join(m.dir, fmt.Sprintf("%d.sock", port)))
	if err != nil {
		return nil, err
	}
	held, err := m.has(holdDialsFile)
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	flood := false
	if port == supervisor.ControlPort {
		if flood, err = m.take(floodFile); err != nil {
			return nil, errors.Join(err, conn.Close())
		}
		every, err := m.has(floodEveryFile)
		if err != nil {
			return nil, errors.Join(err, conn.Close())
		}
		flood = flood || every
		if err := m.appendTo(dialsFile, "control"); err != nil {
			return nil, errors.Join(err, conn.Close())
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if held || time.Now().Before(m.holdUntil) {
		return conn, conn.Close()
	}
	m.streams[conn] = struct{}{}
	if port != supervisor.ControlPort {
		return &stream{Conn: conn, machine: m}, nil
	}
	m.controls++
	if err := m.appendTo(controlsFile, "control"); err != nil {
		delete(m.streams, conn)

		return nil, errors.Join(err, conn.Close())
	}
	s := &stream{Conn: conn, machine: m}
	if flood {
		return &flooded{stream: s}, nil
	}

	return s, nil
}

// dropStreams ends every stream to the guest at once, which is what a reset of the transport looks like to both ends.
func (m *fakeMachine) dropStreams() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for conn := range m.streams {
		_ = conn.Close()
		delete(m.streams, conn)
	}
}

// stream is one tracked guest connection, which forgets itself when the shim ends it.
type stream struct {
	net.Conn
	machine *fakeMachine
	// cut loses the next answer the guest sends and resets every stream, once a freeze asked for it.
	cut atomic.Bool
}

func (s *stream) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"kind":"`+supervisor.KindReseed+`"`) {
		if err := s.machine.note(supervisor.KindReseed); err != nil {
			return 0, err
		}
	}
	for kind, frozen := range map[string]bool{supervisor.KindFreeze: true, supervisor.KindThaw: false} {
		if !strings.Contains(string(p), `"kind":"`+kind+`"`) {
			continue
		}
		if err := errors.Join(s.machine.setFrozen(frozen), s.machine.note(kind)); err != nil {
			return 0, err
		}
		if !frozen {
			continue
		}
		cut, err := s.machine.take(cutFreezeFile)
		if err != nil {
			return 0, err
		}
		s.cut.Store(cut)
	}

	return s.Conn.Write(p)
}

func (s *stream) Read(p []byte) (int, error) {
	n, err := s.Conn.Read(p)
	if s.cut.Load() && strings.Contains(string(p[:n]), `"kind":"`+supervisor.KindDone+`"`) {
		s.machine.dropStreams()

		return 0, net.ErrClosed
	}

	return n, err
}

// flooded passes the guest's state line, then reads as one line that never ends.
type flooded struct {
	*stream
	passed bool
}

func (f *flooded) Read(p []byte) (int, error) {
	if f.passed {
		return copy(p, bytes.Repeat([]byte{'x'}, len(p))), nil
	}
	n, err := f.stream.Read(p)
	if end := bytes.IndexByte(p[:n], '\n'); end >= 0 {
		f.passed = true

		return end + 1, err
	}

	return n, err
}

func (s *stream) Close() error {
	s.machine.mu.Lock()
	delete(s.machine.streams, s.Conn)
	s.machine.mu.Unlock()

	return s.Conn.Close()
}

func (m *fakeMachine) Network() (*os.File, error) {
	return nil, errors.New("the fake shim has no network device")
}
