package firecracker_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/provider/firecracker"
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

// A stop lets the vmm go only once the guest's last report has landed, however soon the VM halts after it (SHARD-290).
func TestAStopWaitsForTheSupervisorsReportToLand(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeInitEnv, self)
	t.Setenv(failingGuestEnv, "1")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A fifo is a disk slower than the halt: the report's write blocks until the test reads it.
	reason := filepath.Join(dir, firecracker.SupervisorFailedFile)
	if err := syscall.Mkfifo(reason, 0o600); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.provider.Stop(context.WithoutCancel(t.Context()), spec.ID, stopGrace) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop = %v while the report was still unwritten, want it to wait for the report", err)
	case <-time.After(time.Second):
	}
	got, err := os.ReadFile(reason)
	if err != nil {
		t.Fatal(err)
	}
	// A status read would block on the fifo, so it goes before the cleanup reads one.
	if err := os.Remove(reason); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop after the report landed = %v", err)
	}
	if want := strings.ReplaceAll(guestFailure, "\n", " "); string(got) != want {
		t.Fatalf("the report wrote %q, want %q", got, want)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != models.SupervisorFailedExitCode {
		t.Fatalf("Wait = %+v, %v, want the supervisor's %d", exit, err, models.SupervisorFailedExitCode)
	}
}

// A report the host could not write fails the stop, so a lost 125 never reads as an ordinary stop (SHARD-290).
func TestAStopFailsWhenTheSupervisorsReportCannotLand(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeInitEnv, self)
	t.Setenv(failingGuestEnv, "1")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	dir, err := h.stateDir(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the reason goes fails its write for root too.
	if err := os.Mkdir(filepath.Join(dir, firecracker.SupervisorFailedFile), 0o700); err != nil {
		t.Fatal(err)
	}

	err = h.provider.Stop(t.Context(), spec.ID, stopGrace)
	if err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Stop = %v, want the lost report", err)
	}

	// The vmm is gone and forgotten, and every later verb still answers with the loss until rm.
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Errorf("a second Stop = %v, want the lost report", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Errorf("Start = %v, want the lost report", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Alive() || !strings.Contains(status.SupervisorFailed, "lost its lifecycle state") {
		t.Errorf("Status = %+v, want a dead sandbox whose supervisor failure names the loss", status)
	}
	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove = %v, want the loss dropped with the sandbox", err)
	}
}
