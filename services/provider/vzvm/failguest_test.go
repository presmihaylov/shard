package vzvm_test

import (
	"bufio"
	"context"
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
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/supervisor"
)

// bootFailingGuestEnv makes the test binary a guest whose boot failed before it could listen for more, as a root disk that will not mount does.
const bootFailingGuestEnv = "VZVM_FAKE_BOOT_FAILING_GUEST"

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

// failingGuestEnv makes the test binary a guest that dies on the stop, as a shard-init whose root will not freeze does.
const failingGuestEnv = "VZVM_FAKE_FAILING_GUEST"

// guestFailure is the reason the failing guest gives, with the newline a guest may put in one.
const guestFailure = "supervisor: freeze the root: operation not supported\nsecond line"

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

// A shard-init that dies after it answered the stop leaves its reason, where the stop dropped the report with the guest (SHARD-476).
func TestAShardInitThatDiesOnTheStopLeavesItsReason(t *testing.T) {
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
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}

	status, err := h.provider.Status(t.Context(), spec.ID)
	want := strings.ReplaceAll(guestFailure, "\n", " ")
	if err != nil || status.State != models.StateStopped || status.SupervisorFailed != want {
		t.Fatalf("Status after the death = %+v, %v, want stopped with the reason %q on one line", status, err, want)
	}
}

// A stop returns only once the guest's last report has landed, however soon the VM halts after it (SHARD-476).
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
	reason := filepath.Join(dir, vzvm.SupervisorFailedFile)
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

// A boot that fails answers the start with its reason and its 125, where the start named only the unexpected opener and recorded nothing (SHARD-418).
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

	// The next boot is shard-init again, and the reason the last one left must not answer for it.
	t.Setenv(fakeInitEnv, initBinary)
	t.Setenv(bootFailingGuestEnv, "")
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
