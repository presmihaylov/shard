package vzvm

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/supervisor"
)

type silentStateMachine struct{ guest net.Conn }

func (m silentStateMachine) State() vz.State                  { return vz.StateRunning }
func (m silentStateMachine) MachineID() string                { return "silent-state" }
func (m silentStateMachine) Pause() error                     { return nil }
func (m silentStateMachine) Resume() error                    { return nil }
func (m silentStateMachine) Save(string) error                { return nil }
func (m silentStateMachine) Stop() error                      { return nil }
func (m silentStateMachine) Connect(uint32) (net.Conn, error) { return m.guest, nil }
func (m silentStateMachine) Network() (*os.File, error) {
	return nil, errors.New("the test guest has no network")
}

func TestAttachDeadlineEndsTheFirstStateWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	assertAttachEnds(t, ctx, context.DeadlineExceeded)
}

func TestAttachCancelEndsTheFirstStateWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(time.Second, cancel)
	defer timer.Stop()
	assertAttachEnds(t, ctx, context.Canceled)
}

func assertAttachEnds(t *testing.T, ctx context.Context, want error) {
	t.Helper()
	host, guest := net.Pipe()
	root, socket := serveShim(t, silentStateMachine{guest: host})
	t.Cleanup(func() {
		for _, conn := range []net.Conn{host, guest} {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	provider := &Provider{cfg: Config{Log: log.New(io.Discard, "", 0)}}
	done := make(chan error, 1)
	go func() {
		_, err := provider.attach(ctx, "silent-state", root, record{}, vz.Open(socket), vz.Info{PID: os.Getpid(), MachineID: "silent-state"}, false, startGrace)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("the silent guest ended the attach with %v, want %v", err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the attach waited for state past the create deadline")
	}
	if err := guest.SetReadDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if _, err := guest.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the create deadline left the guest stream open: %v", err)
	}
}

// serveShim serves machine on a shim socket in a fresh root until the test ends, and returns the root and the socket; a cleanup the caller registers after it runs first, so it can end the guest streams the shim waits on.
func serveShim(t *testing.T, machine vz.Machine) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "vz-state") //nolint:usetesting // t.TempDir exceeds the macOS socket path limit.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.Listen("unix", filepath.Join(root, socketFile))
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- vz.Serve(listener, machine, log.New(io.Discard, "", 0)) }()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		if err := <-served; err != nil {
			t.Error(err)
		}
	})

	return root, listener.Addr().String()
}

// mutedGuestMachine runs, and hands each dial to a guest that takes it and never answers, as a shard-init past its last loop does.
type mutedGuestMachine struct {
	silentStateMachine
	dialed chan net.Conn
}

func (m mutedGuestMachine) Connect(uint32) (net.Conn, error) {
	shim, guest := net.Pipe()
	m.dialed <- guest

	return shim, nil
}

// A stop that found the shim gone ends the redial of a guest that never replays, so settle does not wait out its grace (SHARD-638).
func TestSettleEndsARedialTheGuestNeverAnswers(t *testing.T) {
	dialed := make(chan net.Conn, 16)
	root, socket := serveShim(t, mutedGuestMachine{dialed: dialed})
	t.Cleanup(func() {
		close(dialed)
		for guest := range dialed {
			if err := guest.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	logger := log.New(io.Discard, "", 0)
	provider := &Provider{cfg: Config{Log: logger}}
	m := &machine{id: "sb-1", dir: root, client: vz.Open(socket), events: make(chan struct{}), refusals: supervisor.NewRefusals(logger, "sb-1")}
	m.following, m.unfollow = context.WithCancel(context.Background())
	host, dropped := net.Pipe()
	m.control.Store(supervisor.ControlOver(host))
	go provider.follow(m)

	// A dropped control stream sends the follower to dial it again.
	if err := dropped.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case guest := <-dialed:
		dialed <- guest
	case <-time.After(5 * time.Second):
		t.Fatal("the follower did not dial the control stream again")
	}
	if err := provider.settle(t.Context(), m); err != nil {
		t.Fatal(err)
	}
}
