package vzvm_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/vz"
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
	select {
	case <-signals:
		if err := machine.Stop(); err != nil {
			return err
		}
		<-machine.exited
	case <-machine.exited:
	}
	if err := listener.Close(); err != nil {
		return err
	}

	return <-served
}

// fakeMachine is a shard-init process in its own group: a pause is SIGSTOP, a save a marker file, a stop SIGKILL.
type fakeMachine struct {
	id     string
	dir    string
	cmd    *exec.Cmd
	exited chan struct{}

	mu    sync.Mutex
	state vz.State
}

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
	if cfg.Restore != "" {
		saved, err := os.ReadFile(cfg.Restore)
		if err != nil {
			return nil, fmt.Errorf("read the saved state: %w", err)
		}
		if strings.TrimSpace(string(saved)) != id {
			return nil, fmt.Errorf("the saved state belongs to machine %s, not %s", strings.TrimSpace(string(saved)), id)
		}
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

	m := &fakeMachine{id: id, dir: dir, cmd: cmd, exited: make(chan struct{}), state: vz.StateRunning}
	go func() {
		defer close(m.exited)
		// The exit is the guest powering off; the group may still hold an entrypoint that ignored TERM.
		_ = cmd.Wait()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		m.mu.Lock()
		m.state = vz.StateStopped
		m.mu.Unlock()
	}()

	return m, nil
}

func (m *fakeMachine) State() vz.State {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state
}

func (m *fakeMachine) MachineID() string { return m.id }

func (m *fakeMachine) Pause() error {
	return m.move(vz.StateRunning, vz.StatePaused, syscall.SIGSTOP)
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
	if m.State() != vz.StatePaused {
		return fmt.Errorf("the vm is %s, and only a paused one saves", m.State())
	}

	return os.WriteFile(path, []byte(m.id), 0o600)
}

func (m *fakeMachine) Stop() error {
	if err := syscall.Kill(-m.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

func (m *fakeMachine) Connect(port uint32) (net.Conn, error) {
	return net.Dial("unix", filepath.Join(m.dir, fmt.Sprintf("%d.sock", port)))
}

func (m *fakeMachine) Network() (*os.File, error) {
	return nil, errors.New("the fake shim has no network device")
}
