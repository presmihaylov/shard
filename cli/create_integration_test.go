//go:build integration

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// stopGrace is generous: the entrypoint is already gone by the time the cleanup stops the sandbox.
const stopGrace = 10 * time.Second

// waitBudget bounds the wait for an entrypoint that has already exited. The --user bug spent it all.
const waitBudget = 30 * time.Second

// TestCreateLeavesTheSandboxRunning is the SHARD-16 acceptance criterion, in process. The command
// prints an id and returns, and the sandbox it built outlives it.
func TestCreateLeavesTheSandboxRunning(t *testing.T) {
	app, out := newCreateApp(t)
	deps := createApp(t, app)

	if err := app.Run(t.Context(), []string{"create", testImage, "--", "/bin/sleep", "600"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := onlySandbox(t, app.Root)
	t.Cleanup(func() { cleanUp(t, deps, id) })

	if got := strings.TrimSpace(out.String()); got != id {
		t.Errorf("create printed %q, want the bare id %q", out.String(), id)
	}

	sb, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if sb.State != models.StateRunning {
		t.Errorf("the record says %q, want running: a sandbox outlives its entrypoint", sb.State)
	}

	status, err := deps.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Alive() {
		t.Errorf("the substrate says %+v, want a live sandbox", status)
	}

	if exists, err := netns.NamespaceExists(id); err != nil || !exists {
		t.Errorf("the namespace of %s is gone: %v", id, err)
	}
	if !hasLink(t, sb.HostInterface) {
		t.Errorf("the host interface %s is gone", sb.HostInterface)
	}
	if len(leases(t, app.Root)) != 1 {
		t.Errorf("the leases are %v, want exactly the one this sandbox holds", leases(t, app.Root))
	}
}

// TestCreateOutlivesAnEntrypointThatExits: the keep-alive rule, at the substrate. The entrypoint is
// gone and the sandbox is still there, which is what makes exec worth having.
func TestCreateOutlivesAnEntrypointThatExits(t *testing.T) {
	app, _ := newCreateApp(t)
	deps := createApp(t, app)

	if err := app.Run(t.Context(), []string{"create", testImage, "--", "/bin/true"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := onlySandbox(t, app.Root)
	t.Cleanup(func() { cleanUp(t, deps, id) })

	if status := awaitEntrypoint(t, deps, id); status.Code != 0 {
		t.Errorf("the entrypoint ended %+v, want a clean exit", status)
	}

	status, err := deps.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Alive() {
		t.Errorf("the substrate says %+v after the entrypoint exited, want a live sandbox", status)
	}
}

// TestCreateRunsTheEntrypointAsANonRootUser is the bug this ticket fixes. The user went onto the OCI
// process, which is shard-init, so PID 1 lost the right to write exit.json and Wait polled forever.
func TestCreateRunsTheEntrypointAsANonRootUser(t *testing.T) {
	app, _ := newCreateApp(t)
	deps := createApp(t, app)

	args := []string{"create", "--user", "nobody", testImage, "--", "/bin/sh", "-c", "id -u"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := onlySandbox(t, app.Root)
	t.Cleanup(func() { cleanUp(t, deps, id) })

	// The exit status is the assertion: a supervisor that dropped too could never write it.
	if status := awaitEntrypoint(t, deps, id); status.Code != 0 {
		t.Errorf("the entrypoint ended %+v, want a clean exit", status)
	}

	status, err := deps.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Alive() {
		t.Errorf("the substrate says %+v, want a live sandbox", status)
	}

	if got := guestOutput(t, deps, id); !strings.Contains(got, "65534") {
		t.Errorf("the entrypoint reported uid %q, want 65534", strings.TrimSpace(got))
	}
}

// A --user entrypoint keeps what config.json grants it. The drop happens in the supervisor now, and
// a uid change away from root clears the permitted and the effective set unless they are raised into
// the ambient one, so without that the entrypoint got nothing and bind(80) returned EACCES.
func TestCreateKeepsTheCapabilitiesOfANonRootEntrypoint(t *testing.T) {
	app, _ := newCreateApp(t)
	deps := createApp(t, app)

	args := []string{"create", "--user", "nobody", testImage, "--", "/bin/sh", "-c", "grep CapEff /proc/self/status"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := onlySandbox(t, app.Root)
	t.Cleanup(func() { cleanUp(t, deps, id) })

	if status := awaitEntrypoint(t, deps, id); status.Code != 0 {
		t.Fatalf("the entrypoint ended %+v, want a clean exit", status)
	}

	mask := effectiveCapabilities(t, guestOutput(t, deps, id))
	// CAP_NET_BIND_SERVICE is bit 10. It is in the set the spec grants, so the entrypoint must hold it.
	if mask&(1<<10) == 0 {
		t.Errorf("the entrypoint holds the effective set %#x, want CAP_NET_BIND_SERVICE in it", mask)
	}
}

// TestCreateLeaksNothingWhenItFails is the other half: half-built state is a bug, so a failure at
// any claim gives back the record, the lease, the namespace, the link and the mount.
func TestCreateLeaksNothingWhenItFails(t *testing.T) {
	app, _ := newCreateApp(t)

	// A supervisor that is not there fails the bind mount, which is the last claim before the start.
	app.InitPath = filepath.Join(t.TempDir(), "absent")

	args := []string{"create", testImage, "--", "/bin/true"}
	if err := app.Run(t.Context(), args); err == nil {
		t.Fatal("a missing supervisor returned no error")
	}

	left, err := records(t, app.Root).List()
	if err != nil {
		t.Fatalf("list the records: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("the failed create left the records %+v", left)
	}

	if held := leases(t, app.Root); len(held) != 0 {
		t.Errorf("the failed create left the leases %v", held)
	}

	// The substrate claim is given back too: an interrupted create can leave a sandbox process that
	// only runsc can reach, and the record the teardown deletes is the only handle to it.
	if held := containers(t, app.Root); len(held) != 0 {
		t.Errorf("the failed create left the runsc containers %v", held)
	}

	// Only the sandbox tree: runsc bind mounts a null-netns into its own root on the first create,
	// and that one belongs to the runsc root rather than to any sandbox.
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("read the mount table: %v", err)
	}
	if sandboxes := filepath.Join(app.Root, "sandboxes"); strings.Contains(string(mounts), sandboxes) {
		t.Errorf("the failed create left a mount under %s", sandboxes)
	}
}

// TestCreateGivesEverythingBackWhenTheEntrypointDoesNotStart is the whole point of the handshake.
// runsc create and runsc start both succeed for an entrypoint that does not exist, because the root
// process is the supervisor. Without the handshake create printed an id, wrote running and exited 0,
// and the record, the lease, the namespace, the link and the mount all outlived the sandbox.
func TestCreateGivesEverythingBackWhenTheEntrypointDoesNotStart(t *testing.T) {
	app, out := newCreateApp(t)

	args := []string{"create", testImage, "--", "/no/such/entrypoint"}
	err := app.Run(t.Context(), args)
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

	left, err := records(t, app.Root).List()
	if err != nil {
		t.Fatalf("list the records: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("the failed create left the records %+v", left)
	}

	if held := leases(t, app.Root); len(held) != 0 {
		t.Errorf("the failed create left the leases %v", held)
	}
	if held := containers(t, app.Root); len(held) != 0 {
		t.Errorf("the failed create left the runsc containers %v", held)
	}

	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("read the mount table: %v", err)
	}
	if sandboxes := filepath.Join(app.Root, "sandboxes"); strings.Contains(string(mounts), sandboxes) {
		t.Errorf("the failed create left a mount under %s", sandboxes)
	}
}

// TestCreateLeaksNothingWhenItIsInterrupted: Ctrl-C cancels the command's own context, so the
// give-back has to run on one the interrupt cannot have cancelled.
func TestCreateLeaksNothingWhenItIsInterrupted(t *testing.T) {
	app, _ := newCreateApp(t)

	// The image is pulled first, so the cancellation lands on a create that has claims to give back.
	if err := app.Run(t.Context(), []string{"pull", testImage}); err != nil {
		t.Fatalf("pull: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := app.Run(ctx, []string{"create", testImage, "--", "/bin/true"}); err == nil {
		t.Fatal("an interrupted create returned no error")
	}

	left, err := records(t, app.Root).List()
	if err != nil {
		t.Fatalf("list the records: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("the interrupted create left the records %+v", left)
	}

	if held := leases(t, app.Root); len(held) != 0 {
		t.Errorf("the interrupted create left the leases %v", held)
	}
	if held := containers(t, app.Root); len(held) != 0 {
		t.Errorf("the interrupted create left the runsc containers %v", held)
	}
}

func newCreateApp(t *testing.T) (App, *bytes.Buffer) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("shard create needs root")
	}

	for _, binary := range []string{"runsc", "ip", "nft"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("no %s on this host", binary)
		}
	}

	if _, err := os.Stat(hostInitPath); err != nil {
		t.Skipf("no supervisor at %s: run make devbox-sync first", hostInitPath)
	}

	out := &bytes.Buffer{}
	root := t.TempDir()

	// runsc bind mounts a null-netns into its own root on the first create, and the TempDir removal
	// trips over it. Cleanup is LIFO, so this runs before that removal and after the sandbox is gone.
	t.Cleanup(func() {
		if err := exec.Command("umount", "-l", filepath.Join(root, "runsc", "null-netns")).Run(); err != nil {
			t.Logf("unmount the runsc null-netns: %v", err)
		}
	})

	// No factory override: these tests drive the real wiring, which is the whole point of them.
	app := App{Version: "test", Root: root, Out: out, Err: out, Timeout: 5 * time.Minute, InitPath: hostInitPath}

	return app, out
}

// createApp builds a second view of the same layers, so the test can stop what create left running.
func createApp(t *testing.T, app App) createDeps {
	t.Helper()

	deps, err := defaultCreateDeps(app)
	if err != nil {
		t.Fatalf("build the create dependencies: %v", err)
	}

	return deps
}

// awaitEntrypoint waits for the exit status shard-init writes. It is bounded, because the bug this
// ticket fixes made that file never arrive and the wait then never returned.
func awaitEntrypoint(t *testing.T, deps createDeps, id string) models.ExitStatus {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	status, err := deps.provider.Wait(ctx, id)
	if err != nil {
		t.Fatalf("wait for the entrypoint of %s: %v", id, err)
	}

	return status
}

func guestOutput(t *testing.T, deps createDeps, id string) string {
	t.Helper()

	path, err := deps.provider.LogPath(id)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(blob)
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

// cleanUp ends the sandbox the test left running and gives its address back. It builds its own
// context, because the test's own is already cancelled by the time a cleanup runs.
func cleanUp(t *testing.T, deps createDeps, id string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := deps.provider.Stop(ctx, id, stopGrace); err != nil {
		t.Logf("stop %s: %v", id, err)
	}
	if err := deps.provider.Remove(ctx, id); err != nil {
		t.Logf("remove %s: %v", id, err)
	}
	if err := deps.net.Release(ctx, id); err != nil {
		t.Logf("release the network of %s: %v", id, err)
	}
	if err := deps.repo.Delete(id); err != nil {
		t.Logf("delete the record of %s: %v", id, err)
	}
}

// records reads the state tree create wrote, rather than reaching back through its own repository.
func records(t *testing.T, root string) *sandboxstate.Repository {
	t.Helper()

	repo, err := sandboxstate.New(root)
	if err != nil {
		t.Fatalf("open the state repository: %v", err)
	}

	return repo
}

func onlySandbox(t *testing.T, root string) string {
	t.Helper()

	left, err := records(t, root).List()
	if err != nil {
		t.Fatalf("list the records: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("create left %d records, want one", len(left))
	}

	return left[0].ID
}

func leases(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "network", "leases"))
	if err != nil {
		t.Fatalf("read the leases: %v", err)
	}

	held := make([]string, 0, len(entries))
	for _, entry := range entries {
		held = append(held, entry.Name())
	}

	return held
}

// containers is what runsc holds under root. null-netns is the bind mount runsc makes for itself on
// the first create, and it belongs to no sandbox.
func containers(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "runsc"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the runsc root: %v", err)
	}

	held := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == "null-netns" {
			continue
		}

		held = append(held, entry.Name())
	}

	return held
}

func hasLink(t *testing.T, name string) bool {
	t.Helper()

	manager, err := netns.New()
	if err != nil {
		t.Fatalf("open the netns manager: %v", err)
	}

	exists, err := manager.LinkExists(t.Context(), name)
	if err != nil {
		t.Fatalf("LinkExists: %v", err)
	}

	return exists
}
