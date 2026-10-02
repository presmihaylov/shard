package vzvm_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/supervisor"
)

// The test binary plays the shim when the provider execs it with this set; the guest is the real shard-init over unix sockets.
const (
	fakeShimEnv = "VZVM_FAKE_SHIM"
	fakeInitEnv = "VZVM_FAKE_INIT"
)

// initBinary is the shard-init the fake shim runs in place of a VM, built once per test run unless the env names one.
var initBinary string

func TestMain(m *testing.M) {
	if os.Getenv(fakeShimEnv) == "1" {
		if err := fakeShim(); err != nil {
			fmt.Fprintln(os.Stderr, "fake shim:", err)
			os.Exit(1)
		}

		return
	}

	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
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
	// Every shim the provider starts from here is this binary, and inherits the switch.
	os.Setenv(fakeShimEnv, "1")
	os.Setenv(fakeInitEnv, initBinary)

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

	listener, err := vz.Listen(cfg.Socket)
	if err != nil {
		return err
	}
	machine, err := bootFake(cfg)
	if err != nil {
		return errors.Join(err, listener.Close())
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	served := make(chan error, 1)
	go func() { served <- vz.Serve(listener, machine, logger) }()

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
			if err := machine.Stop(); err != nil {
				return err
			}
			<-machine.exited

			return errors.Join(listener.Close(), <-served)
		case <-machine.exited:
			return errors.Join(listener.Close(), <-served)
		}
	}
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

// orderFile in the state directory, once a test creates it, takes one line per freeze, reseed and thaw in the order the guest reads them.
const orderFile = "control-order"

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
func (m *fakeMachine) note(kind string) error {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(m.dir), orderFile), os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(kind + "\n")

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
	m.mu.Lock()
	defer m.mu.Unlock()
	if held || time.Now().Before(m.holdUntil) {
		return conn, conn.Close()
	}
	m.streams[conn] = struct{}{}
	if port == supervisor.ControlPort {
		m.controls++
	}

	return &stream{Conn: conn, machine: m}, nil
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

func (s *stream) Close() error {
	s.machine.mu.Lock()
	delete(s.machine.streams, s.Conn)
	s.machine.mu.Unlock()

	return s.Conn.Close()
}

func (m *fakeMachine) Network() (*os.File, error) {
	return nil, errors.New("the fake shim has no network device")
}
