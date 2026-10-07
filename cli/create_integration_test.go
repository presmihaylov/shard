//go:build integration

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/sandbox"
)

// TestCreateLeavesTheSandboxRunning is the SHARD-16 acceptance criterion, in process. The command
// prints an id and returns, and the sandbox it built outlives it.
func TestCreateLeavesTheSandboxRunning(t *testing.T) {
	app, out := newCreateApp(t)

	id := printedID(t, app, out, createArgs(testImage))
	t.Cleanup(func() { cleanUp(t, app, id) })

	if strings.ContainsAny(id, " \t") {
		t.Errorf("create printed %q, want the bare id and nothing else", id)
	}

	sb := record(t, app, id)
	if sb.State != models.StateRunning {
		t.Errorf("the record says %q, want running: a sandbox outlives its processes", sb.State)
	}

	if got, err := runExec(t, app, "exec", id, "/bin/echo", "alive"); err != nil || !strings.Contains(got, "alive") {
		t.Errorf("an exec in the new sandbox wrote %q and failed with %v, want a live sandbox", got, err)
	}

	if exists, err := netns.NamespaceExists(id); err != nil || !exists {
		t.Errorf("the namespace of %s is gone: %v", id, err)
	}
	if !hasLink(t, sb.HostInterface) {
		t.Errorf("the host interface %s is gone", sb.HostInterface)
	}
	if held := leases(t, app.Root); !slices.Contains(held, id) {
		t.Errorf("the leases are %v, want the one this sandbox holds", held)
	}
}

// TestCreateOutlivesAProcessThatExits: the keep-alive rule, at the substrate. The process is
// gone and the sandbox is still there, which is what makes exec worth having.
func TestCreateOutlivesAProcessThatExits(t *testing.T) {
	app, out := newCreateApp(t)

	id := runDetached(t, app, out, "/bin/true")
	t.Cleanup(func() { cleanUp(t, app, id) })

	if status := awaitProcess(t, app, id); status.Code != 0 {
		t.Errorf("the process ended %+v, want a clean exit", status)
	}

	if sb := record(t, app, id); sb.State != models.StateRunning {
		t.Errorf("the record says %q after the process exited, want running", sb.State)
	}
	if got, err := runExec(t, app, "exec", id, "/bin/echo", "alive"); err != nil || !strings.Contains(got, "alive") {
		t.Errorf("an exec after the process exited wrote %q and failed with %v, want a live sandbox", got, err)
	}
}

// TestRunStartsAProcessAsANonRootUser: the user goes onto the process and never onto shard-init, which keeps the right to report it.
func TestRunStartsAProcessAsANonRootUser(t *testing.T) {
	app, out := newCreateApp(t)

	id := runDetachedWith(t, app, out, nil, []string{"--restart", "no", "-u", "nobody"}, "/bin/sh", "-c", "id -u")
	t.Cleanup(func() { cleanUp(t, app, id) })

	// The exit status is the assertion: a supervisor that dropped too could never report it.
	if status := awaitProcess(t, app, id); status.Code != 0 {
		t.Errorf("the process ended %+v, want a clean exit", status)
	}

	if got := guestOutput(t, app, id); !strings.Contains(got, "65534") {
		t.Errorf("the process reported uid %q, want 65534", strings.TrimSpace(got))
	}
}

// A non-root process keeps what config.json grants: a uid change away from root clears the effective set unless the supervisor raises it into the ambient one.
func TestRunKeepsTheCapabilitiesOfANonRootProcess(t *testing.T) {
	app, out := newCreateApp(t)

	id := runDetachedWith(t, app, out, nil, []string{"--restart", "no", "-u", "nobody"}, "/bin/sh", "-c", "grep CapEff /proc/self/status")
	t.Cleanup(func() { cleanUp(t, app, id) })

	if status := awaitProcess(t, app, id); status.Code != 0 {
		t.Fatalf("the process ended %+v, want a clean exit", status)
	}

	mask := effectiveCapabilities(t, guestOutput(t, app, id))
	// CAP_NET_BIND_SERVICE is bit 10. It is in the set the spec grants, so the process must hold it.
	if mask&(1<<10) == 0 {
		t.Errorf("the process holds the effective set %#x, want CAP_NET_BIND_SERVICE in it", mask)
	}
}

// TestCreateThatFailsLeavesOnlyAFailedRecord: a failure at any claim gives back the lease, the
// namespace, the link and the mount, and leaves one failed record that remove then frees.
func TestCreateThatFailsLeavesOnlyAFailedRecord(t *testing.T) {
	// A supervisor that is not there fails the bind mount, the last claim before the start.
	absent := filepath.Join(t.TempDir(), "absent")
	app, _ := ownDaemon(t, InitPathEnv+"="+absent)

	err := app.Run(t.Context(), createArgs(testImage))
	if err == nil {
		t.Fatal("a missing supervisor returned no error")
	}

	// firecracker reads the supervisor into its initrd when the daemon builds it, so the create is refused before any record.
	if itestProvider == firecracker.Name {
		if !strings.Contains(err.Error(), absent) {
			t.Errorf("the refused create failed with %v, want it to name the supervisor %s", err, absent)
		}
		if held := holdings(t, app); len(held) != 0 {
			t.Errorf("the refused create left %v, want nothing", held)
		}
		assertNoSandboxMounts(t, app.Root)

		return
	}

	held := holdings(t, app)
	if len(held) != 1 || !strings.HasPrefix(held[0], "record:") {
		t.Fatalf("the failed create left %v, want a single failed record", held)
	}
	assertNoSandboxMounts(t, app.Root)

	id := strings.TrimPrefix(held[0], "record:")
	if got := record(t, app, id); got.State != models.StateFailed {
		t.Errorf("the leftover record is %q, want failed", got.State)
	}

	// remove frees the failed record: it holds no live process, so the record and everything under it goes.
	cleanUp(t, app, id)
	if held := holdings(t, app); len(held) != 0 {
		t.Errorf("after remove the host holds %v, want nothing", held)
	}
}

// A missing command is the caller's command_not_started, with the shell's 127, and the sandbox it was run in stays up (SHARD-497).
func TestRunOfACommandThatDoesNotStartExits127(t *testing.T) {
	app, out := newCreateApp(t)

	id := printedID(t, app, out, createArgs(testImage))
	t.Cleanup(func() { cleanUp(t, app, id) })

	running, _ := ownStderr(app)
	err := running.Run(t.Context(), []string{"run", id, "--name", processName, "--", "/no/such/entrypoint"})
	exit, ok := errors.AsType[*ExitError](err)
	if !ok || exit.Code != models.CommandNotFoundExitCode || !strings.Contains(err.Error(), "could not run") {
		t.Fatalf("run failed with %v, want command_not_started with exit code %d", err, models.CommandNotFoundExitCode)
	}

	// A run that failed prints no name.
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Errorf("the failed run printed %q, want nothing", got)
	}

	sb := record(t, app, id)
	if sb.State != models.StateRunning {
		t.Errorf("the record says %q after a run that never started, want running", sb.State)
	}
	if p := processOf(t, sb); p.Status.State != models.ProcessExited || p.Status.Exit == nil || p.Status.Exit.Code != models.CommandNotFoundExitCode {
		t.Errorf("the record holds %+v, want the process exited with %d", p.Status, models.CommandNotFoundExitCode)
	}
}

// TestCreateFinishesWhenTheClientGivesUpWaiting: an uncached create runs in the daemon under its own run
// context, not the caller's, so a client that cancels its wait cannot abort the create or leave
// half-built state. The sandbox still reaches running.
func TestCreateFinishesWhenTheClientGivesUpWaiting(t *testing.T) {
	// A daemon of this test's own has an empty image tree, so the image is uncached and the create goes async.
	app, _ := ownDaemon(t)

	sb, err := daemonClient(app).CreateSandbox(t.Context(), sandbox.CreateRequest{
		Image:     testImage,
		Resources: itestResources(),
	})
	if err != nil {
		t.Fatalf("create from an uncached image: %v", err)
	}
	if sb.State != models.StatePending {
		t.Fatalf("the create answered %q, want pending", sb.State)
	}

	// The client gives up on the wait at once; the daemon keeps building behind the record.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := daemonClient(app).WaitSandbox(ctx, sb.ID); err == nil {
		t.Fatal("a cancelled wait returned no error")
	}

	// The sandbox still reaches running, with nothing half-built left behind.
	deadline := time.Now().Add(waitBudget)
	for {
		got := record(t, app, sb.ID)
		if got.State == models.StateRunning {
			return
		}
		if got.State == models.StateFailed {
			t.Fatalf("the sandbox failed after the client left: %s", got.FailedReason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox stayed %q after the client left", got.State)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// assertNoSandboxMounts checks the sandbox tree only: runsc bind mounts a null-netns into its own
// root on the first create, and that one belongs to the runsc root rather than to any sandbox.
func assertNoSandboxMounts(t *testing.T, root string) {
	t.Helper()

	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("read the mount table: %v", err)
	}
	if sandboxes := filepath.Join(root, "sandboxes"); strings.Contains(string(mounts), sandboxes) {
		t.Errorf("the create left a mount under %s", sandboxes)
	}
}

// effectiveCapabilities reads the one hex word the guest printed out of /proc/self/status.
func effectiveCapabilities(t *testing.T, output string) uint64 {
	t.Helper()

	for line := range strings.Lines(output) {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}

		mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		if err != nil {
			t.Fatalf("the guest printed an unreadable CapEff %q: %v", line, err)
		}

		return mask
	}

	t.Fatalf("the guest printed %q, which names no CapEff", output)

	return 0
}
