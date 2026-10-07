package firecracker_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

	// A restore's refreeze speaks first and hangs up, so every host connection is answered in turn.
	for {
		conn, err := control.Accept()
		if err != nil {
			return fmt.Errorf("accept control: %w", err)
		}
		if err := answerUntilStop(conn); !errors.Is(err, io.EOF) {
			return err
		}
	}
}

// answerUntilStop sends the state, answers every request, and reports the death on the stop.
func answerUntilStop(conn net.Conn) error {
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

// A shard-init that dies on the stop reports why before the VM halts: its reason is the stopped status, until the next boot (SHARD-290).
func TestAShardInitThatDiesLeavesItsReason(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t)
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
	spec := h.newSpec(t)
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
}

// A report the host could not write fails the stop, so a lost reason never reads as an ordinary stop (SHARD-290).
func TestAStopFailsWhenTheSupervisorsReportCannotLand(t *testing.T) {
	h := newHarness(t)
	spec := h.lostReport(t)

	// The vmm is gone and forgotten, and Stop and Status answer with the loss until a start boots a fresh run (SHARD-578).
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Errorf("a second Stop = %v, want the lost report", err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Alive() || !strings.Contains(status.SupervisorFailed, "lost its lifecycle state") {
		t.Errorf("Status = %+v, want a dead sandbox whose supervisor failure names the loss", status)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Errorf("Start = %v, want a fresh run past the loss", err)
	}
	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove = %v, want the loss dropped with the sandbox", err)
	}
}

// A start answers a loss the same whether the daemon that held it restarted or not: it boots a fresh run (SHARD-578).
func TestAStartAfterALostReportIsTheSameAcrossARestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			h := newHarness(t)
			spec := h.lostReport(t)
			if restart {
				h.reopen(t)
			}

			if err := h.provider.Start(t.Context(), spec.ID); err != nil {
				t.Fatalf("Start = %v, want a fresh run past the loss", err)
			}
			status, err := h.provider.Status(t.Context(), spec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !status.Alive() || status.SupervisorFailed != "" {
				t.Errorf("Status after Start = %+v, want a live sandbox with no loss", status)
			}
			if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
				t.Fatalf("Remove = %v", err)
			}
		})
	}
}

// lostReport runs a sandbox whose guest dies on the stop with a report the host cannot write, and stops it.
func (h *harness) lostReport(t *testing.T) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t)
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
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Stop = %v, want the lost report", err)
	}

	return spec
}

// bootFailingGuestEnv makes the test binary a guest whose boot failed before it could listen for more, as a root disk that will not mount does.
const bootFailingGuestEnv = "FIRECRACKER_FAKE_BOOT_FAILING_GUEST"

// bootFailure is the reason the boot failing guest gives.
const bootFailure = "the supervisor failed: mount /dev/vdb on /overlay: read-only file system"

// bootFailingGuest opens the control connection with its death in place of its state, and holds on until the host hangs up, as shard-init does.
func bootFailingGuest(dir string) error {
	control, err := net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.ControlPort)))
	if err != nil {
		return fmt.Errorf("listen for control: %w", err)
	}
	conn, err := control.Accept()
	if err != nil {
		return fmt.Errorf("accept control: %w", err)
	}
	report := supervisor.Message{Kind: supervisor.KindSupervisorFailed, Error: bootFailure, Exit: &models.ExitStatus{Code: models.SupervisorFailedExitCode}}
	if err := supervisor.WriteMessage(conn, report); err != nil {
		return fmt.Errorf("report the death: %w", err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return fmt.Errorf("wait for the host to hang up: %w", err)
	}

	return nil
}

// A boot that fails answers the start at once with its reason and its 125, where the start waited out its 30s grace for a state (SHARD-416).
func TestABootFailureAnswersTheStartWithItsReason(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeInitEnv, self)
	t.Setenv(bootFailingGuestEnv, "1")

	began := time.Now()
	err = h.provider.Start(t.Context(), spec.ID)
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("Start answered after %s, want well inside the 30s grace", took)
	}
	if err == nil || !strings.Contains(err.Error(), bootFailure) || !strings.Contains(err.Error(), fmt.Sprintf("exit %d", models.SupervisorFailedExitCode)) {
		t.Fatalf("Start = %v, want the boot's reason and exit %d", err, models.SupervisorFailedExitCode)
	}

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.Alive() || status.SupervisorFailed != bootFailure {
		t.Fatalf("Status after the failed boot = %+v, %v, want a dead sandbox with the reason %q", status, err, bootFailure)
	}
}
