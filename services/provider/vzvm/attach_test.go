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
	host, guest := net.Pipe()
	logger := log.New(io.Discard, "", 0)
	served := make(chan error, 1)
	go func() { served <- vz.Serve(listener, silentStateMachine{guest: host}, logger) }()
	t.Cleanup(func() {
		for _, conn := range []net.Conn{host, guest} {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	provider := &Provider{cfg: Config{Log: logger}}
	done := make(chan error, 1)
	go func() {
		_, err := provider.attach(ctx, "silent-state", root, record{}, vz.Open(listener.Addr().String()), vz.Info{PID: os.Getpid(), MachineID: "silent-state"}, false)
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
