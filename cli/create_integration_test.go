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

// TestCreateLeaksNothingWhenItFails is the other half: half-built state is a bug, so a failure at
// any claim gives back the record, the lease, the namespace, the link and the mount.
func TestCreateLeaksNothingWhenItFails(t *testing.T) {
	// A supervisor that is not there fails the bind mount, which is the last claim before the start.
	app, _ := ownDaemon(t, InitPathEnv+"="+filepath.Join(t.TempDir(), "absent"))

	if err := app.Run(t.Context(), []string{"create", testImage, "--", "/bin/true"}); err == nil {
		t.Fatal("a missing supervisor returned no error")
	}

	if held := holdings(t, app); len(held) != 0 {
		t.Errorf("the failed create left %v", held)
	}
	assertNoSandboxMounts(t, app.Root)
}

// TestCreateGivesEverythingBackWhenTheEntrypointDoesNotStart is the whole point of the handshake.
// runsc create and runsc start both succeed for an entrypoint that does not exist, because the root
// process is the supervisor. Without the handshake create printed an id, wrote running and exited 0,
// and the record, the lease, the namespace, the link and the mount all outlived the sandbox.
func TestCreateGivesEverythingBackWhenTheEntrypointDoesNotStart(t *testing.T) {
	app, out := newCreateApp(t)

	before := holdings(t, app)

	err := app.Run(t.Context(), []string{"create", testImage, "--", "/no/such/entrypoint"})
	if err == nil {
		t.Fatal("create reported success for an entrypoint the image does not hold")
	}
	if !strings.Contains(err.Error(), "did not start") {
		t.Errorf("create failed with %v, want it to say the entrypoint did not start", err)
	}

	// A create that failed prints no id: nothing reachable was left behind to name.
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Errorf("the failed create printed %q, want nothing", got)
	}

	if after := holdings(t, app); !slices.Equal(after, before) {
		t.Errorf("the failed create left %v, want the %v the host held before it", after, before)
	}
	assertNoSandboxMounts(t, app.Root)
}

// TestCreateLeaksNothingWhenItIsInterrupted: Ctrl-C ends the client's request, which cancels the
// daemon's, so the give-back has to run on a context the client cannot have cancelled.
func TestCreateLeaksNothingWhenItIsInterrupted(t *testing.T) {
	app, _ := newCreateApp(t)

	// The image is pulled first, so the cancellation lands on a create that has claims to give back.
	if err := app.Run(t.Context(), []string{"pull", testImage}); err != nil {
		t.Fatalf("pull: %v", err)
	}

	before := holdings(t, app)

	// The interrupt lands once the lease is claimed: the create is then inside the substrate.
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for ctx.Err() == nil {
			if len(leases(t, app.Root)) > len(before) {
				cancel()
			}
			time.Sleep(time.Millisecond)
		}
	}()

	if err := app.Run(ctx, []string{"create", testImage, "--", "/bin/sleep", "600"}); err == nil {
		t.Fatal("an interrupted create returned no error")
	}

	// The daemon gives everything back after the client is gone, so the check waits for it.
	deadline := time.Now().Add(waitBudget)
	for {
		after := holdings(t, app)
		if slices.Equal(after, before) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the interrupted create left %v, want the %v the host held before it", after, before)
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
