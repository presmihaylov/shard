package vzvm_test

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
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

// A boot that fails answers the start with its reason and its 125, where the start named only the unexpected opener and recorded nothing (SHARD-418).
func TestABootFailureAnswersTheStartWithItsReason(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
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
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != models.SupervisorFailedExitCode {
		t.Fatalf("Wait = %+v, %v, want the supervisor's %d", exit, err, models.SupervisorFailedExitCode)
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
