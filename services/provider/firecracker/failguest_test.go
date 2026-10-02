package firecracker_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// failingGuestEnv makes the test binary the guest in place of shard-init: one that dies on the stop, as a supervisor that cannot forward it does.
const failingGuestEnv = "FIRECRACKER_FAKE_FAILING_GUEST"

// guestFailure is the reason the failing guest gives, with the newline a guest may put in one.
const guestFailure = "supervisor: forward the stop to the entrypoint: operation not permitted\nsecond line"

// failingGuest speaks the control protocol as shard-init does until the stop, then reports its own death.
func failingGuest(dir string) error {
	control, err := net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.ControlPort)))
	if err != nil {
		return fmt.Errorf("listen for control: %w", err)
	}
	logs, err := net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.LogsPort)))
	if err != nil {
		return fmt.Errorf("listen for logs: %w", err)
	}
	// The host keeps a logs connection open for the life of the guest; held, so no finalizer closes it.
	held := make(chan net.Conn, 64)
	go func() {
		for {
			conn, err := logs.Accept()
			if err != nil {
				return
			}
			held <- conn
		}
	}()

	conn, err := control.Accept()
	if err != nil {
		return fmt.Errorf("accept control: %w", err)
	}
	if err := supervisor.WriteMessage(conn, supervisor.Message{Kind: supervisor.KindState, Logs: supervisor.LogsVersion}); err != nil {
		return fmt.Errorf("send the state: %w", err)
	}
	r := bufio.NewReader(conn)
	for {
		var m supervisor.Message
		if err := supervisor.ReadMessage(r, &m); err != nil {
			return fmt.Errorf("read a request: %w", err)
		}
		if err := supervisor.WriteMessage(conn, supervisor.Message{Kind: supervisor.KindDone, ID: m.ID}); err != nil {
			return fmt.Errorf("answer %s: %w", m.Kind, err)
		}
		if m.Kind != supervisor.KindStop {
			continue
		}
		report := supervisor.Message{Kind: supervisor.KindSupervisorFailed, Error: guestFailure, Exit: &models.ExitStatus{Code: models.SupervisorFailedExitCode}}
		if err := supervisor.WriteMessage(conn, report); err != nil {
			return fmt.Errorf("report the death: %w", err)
		}
		// The fake vmm proxies the stream and dies with the guest, so the report needs a moment through it first.
		time.Sleep(200 * time.Millisecond)

		return nil
	}
}

// A shard-init that dies on the stop reports why before the VM halts: its 125 is the sandbox exit and its reason the stopped status, until the next boot (SHARD-290).
func TestAShardInitThatDiesLeavesItsExitAndItsReason(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Create boots the VM, so the guest is chosen before it.
	t.Setenv(fakeInitEnv, self)
	t.Setenv(failingGuestEnv, "1")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}

	status, err := h.provider.Status(t.Context(), spec.ID)
	want := strings.ReplaceAll(guestFailure, "\n", " ")
	if err != nil || status.State != models.StateStopped || status.SupervisorFailed != want {
		t.Fatalf("Status after the death = %+v, %v, want stopped with the reason %q on one line", status, err, want)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != models.SupervisorFailedExitCode {
		t.Fatalf("Wait = %+v, %v, want the supervisor's %d", exit, err, models.SupervisorFailedExitCode)
	}

	// The next boot is shard-init again, and the reason the last one left must not answer for it.
	t.Setenv(fakeInitEnv, initBinary)
	t.Setenv(failingGuestEnv, "")
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped || status.SupervisorFailed != "" {
		t.Fatalf("Status after a clean stop = %+v, %v, want stopped with no reason", status, err)
	}
}
