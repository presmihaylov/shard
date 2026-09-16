//go:build integration

package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/sandbox"
)

// TestCreateLeavesTheSandboxRunning is the SHARD-16 acceptance criterion, in process. The command
// prints an id and returns, and the sandbox it built outlives it.
func TestCreateLeavesTheSandboxRunning(t *testing.T) {
	app, out := newCreateApp(t)

	if err := app.Run(t.Context(), []string{"create", testImage, "--", "/bin/sleep", "600"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(out.String())
	t.Cleanup(func() { cleanUp(t, app, id) })

	if strings.ContainsAny(id, " \t") {
		t.Errorf("create printed %q, want the bare id and nothing else", id)
	}

	sb := record(t, app, id)
	if sb.State != models.StateRunning {
		t.Errorf("the record says %q, want running: a sandbox outlives its entrypoint", sb.State)
	}

	if got, err := runExec(t, app, "exec", id, "--", "/bin/echo", "alive"); err != nil || !strings.Contains(got, "alive") {
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

// TestCreateOutlivesAnEntrypointThatExits: the keep-alive rule, at the substrate. The entrypoint is
// gone and the sandbox is still there, which is what makes exec worth having.
func TestCreateOutlivesAnEntrypointThatExits(t *testing.T) {
	app, out := newCreateApp(t)

	id := create(t, app, out, "/bin/true")
	t.Cleanup(func() { cleanUp(t, app, id) })

	if status := awaitEntrypoint(t, app, id); status.Code != 0 {
		t.Errorf("the entrypoint ended %+v, want a clean exit", status)
	}

	if sb := record(t, app, id); sb.State != models.StateRunning {
		t.Errorf("the record says %q after the entrypoint exited, want running", sb.State)
	}
	if got, err := runExec(t, app, "exec", id, "--", "/bin/echo", "alive"); err != nil || !strings.Contains(got, "alive") {
		t.Errorf("an exec after the entrypoint exited wrote %q and failed with %v, want a live sandbox", got, err)
	}
}

// TestCreateRunsTheEntrypointAsANonRootUser is the bug this ticket fixes. The user went onto the OCI
// process, which is shard-init, so PID 1 lost the right to write exit.json and Wait polled forever.
func TestCreateRunsTheEntrypointAsANonRootUser(t *testing.T) {
	app, out := newCreateApp(t)

	if err := app.Run(t.Context(), []string{"create", "--user", "nobody", testImage, "--", "/bin/sh", "-c", "id -u"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(out.String())
	t.Cleanup(func() { cleanUp(t, app, id) })

	// The exit status is the assertion: a supervisor that dropped too could never write it.
	if status := awaitEntrypoint(t, app, id); status.Code != 0 {
		t.Errorf("the entrypoint ended %+v, want a clean exit", status)
	}

	if got := guestOutput(t, app, id); !strings.Contains(got, "65534") {
		t.Errorf("the entrypoint reported uid %q, want 65534", strings.TrimSpace(got))
	}
}

// A --user entrypoint keeps what config.json grants it. The drop happens in the supervisor now, and
// a uid change away from root clears the permitted and the effective set unless they are raised into
// the ambient one, so without that the entrypoint got nothing and bind(80) returned EACCES.
func TestCreateKeepsTheCapabilitiesOfANonRootEntrypoint(t *testing.T) {
	app, out := newCreateApp(t)

	args := []string{"create", "--user", "nobody", testImage, "--", "/bin/sh", "-c", "grep CapEff /proc/self/status"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(out.String())
	t.Cleanup(func() { cleanUp(t, app, id) })

	if status := awaitEntrypoint(t, app, id); status.Code != 0 {
		t.Fatalf("the entrypoint ended %+v, want a clean exit", status)
	}

	mask := effectiveCapabilities(t, guestOutput(t, app, id))
	// CAP_NET_BIND_SERVICE is bit 10. It is in the set the spec grants, so the entrypoint must hold it.
	if mask&(1<<10) == 0 {
		t.Errorf("the entrypoint holds the effective set %#x, want CAP_NET_BIND_SERVICE in it", mask)
	}
}

// TestCreateThatFailsLeavesOnlyAFailedRecord: a failure at any claim gives back the lease, the
// namespace, the link and the mount, and leaves one failed record that rm then frees.
func TestCreateThatFailsLeavesOnlyAFailedRecord(t *testing.T) {
	// A supervisor that is not there fails the bind mount, the last claim before the start.
	app, _ := ownDaemon(t, InitPathEnv+"="+filepath.Join(t.TempDir(), "absent"))

	if err := app.Run(t.Context(), []string{"create", testImage, "--", "/bin/true"}); err == nil {
		t.Fatal("a missing supervisor returned no error")
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

	// rm frees the failed record: it holds no live process, so the record and everything under it goes.
	cleanUp(t, app, id)
	if held := holdings(t, app); len(held) != 0 {
		t.Errorf("after rm the host holds %v, want nothing", held)
	}
}

// TestCreateWhoseEntrypointDoesNotStartLeavesOnlyAFailedRecord: runsc create and start both succeed
// for a missing entrypoint, because the root process is the supervisor. The handshake catches it, so
// create fails, prints no id, frees the lease, namespace, link and mount, and leaves a failed record.
func TestCreateWhoseEntrypointDoesNotStartLeavesOnlyAFailedRecord(t *testing.T) {
	app, out := newCreateApp(t)

	before := holdings(t, app)

	err := app.Run(t.Context(), []string{"create", testImage, "--", "/no/such/entrypoint"})
	if err == nil {
		t.Fatal("create reported success for an entrypoint the image does not hold")
	}
	if !strings.Contains(err.Error(), "did not start") {
		t.Errorf("create failed with %v, want it to say the entrypoint did not start", err)
	}

	// A create that failed prints no id.
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Errorf("the failed create printed %q, want nothing", got)
	}

	added := addedHoldings(before, holdings(t, app))
	if len(added) != 1 || !strings.HasPrefix(added[0], "record:") {
		t.Fatalf("the failed create left %v beyond a single failed record", added)
	}
	assertNoSandboxMounts(t, app.Root)

	id := strings.TrimPrefix(added[0], "record:")
	if got := record(t, app, id); got.State != models.StateFailed {
		t.Errorf("the leftover record is %q, want failed", got.State)
	}

	// rm frees the failed record and the host is back to what it held before the create.
	cleanUp(t, app, id)
	if got := holdings(t, app); !slices.Equal(got, before) {
		t.Errorf("after rm the host holds %v, want the %v it held before", got, before)
	}
}

// TestCreateFinishesWhenTheClientGivesUpWaiting: an uncached create runs in the daemon under its own run
// context, not the caller's, so a client that cancels its wait cannot abort the create or leave
// half-built state. The sandbox still reaches running.
func TestCreateFinishesWhenTheClientGivesUpWaiting(t *testing.T) {
	// A daemon of this test's own has an empty image tree, so the image is uncached and the create goes async.
	app, _ := ownDaemon(t)

	sb, err := daemonClient(app).CreateSandbox(t.Context(), sandbox.CreateRequest{
		Image:   testImage,
		Command: []string{"/bin/sleep", "600"},
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
