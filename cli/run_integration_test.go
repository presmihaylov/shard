//go:build integration

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// TestRunLeavesTheSandboxRunning is the SHARD-16 acceptance criterion, in process. The command
// streams the guest's output, reports the entrypoint's exit code, and the sandbox outlives it.
func TestRunLeavesTheSandboxRunning(t *testing.T) {
	app, out := newRunApp(t)
	deps := runApp(t, app, hostInitPath)

	err := app.Run(t.Context(), []string{"run", testImage, "--", "/bin/echo", "hello"})

	var exit ExitError
	if !errors.As(err, &exit) || exit.Code != 0 {
		t.Fatalf("run: %v, want an exit code of 0", err)
	}

	if !strings.Contains(out.String(), "hello") {
		t.Errorf("the guest output was not streamed: %q", out.String())
	}

	id := onlySandbox(t, app.Root)
	t.Cleanup(func() { cleanUp(t, deps, id) })

	sb, err := records(t, app.Root).Get(id)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	if sb.State != models.StateRunning {
		t.Errorf("the record says %q, want running: a sandbox outlives its entrypoint", sb.State)
	}
	if sb.ExitStatus == nil || sb.ExitStatus.Code != 0 {
		t.Errorf("the record holds %+v, want the entrypoint's exit status", sb.ExitStatus)
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

// TestRunLeaksNothingWhenCreateFails is the other half: half-built state is a bug, so a failure at
// any claim gives back the record, the lease, the namespace, the link and the mount.
func TestRunLeaksNothingWhenCreateFails(t *testing.T) {
	app, _ := newRunApp(t)

	// A supervisor that is not there fails the bind mount, which is the last claim before the start.
	args := []string{"run", "--shard-init", filepath.Join(t.TempDir(), "absent"), testImage, "--", "/bin/true"}
	if err := app.Run(t.Context(), args); err == nil {
		t.Fatal("a missing supervisor returned no error")
	}

	left, err := records(t, app.Root).List()
	if err != nil {
		t.Fatalf("list the records: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("the failed run left the records %+v", left)
	}

	if held := leases(t, app.Root); len(held) != 0 {
		t.Errorf("the failed run left the leases %v", held)
	}

	// Only the sandbox tree: runsc bind mounts a null-netns into its own root on the first create,
	// and that one belongs to the runsc root rather than to any sandbox.
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Fatalf("read the mount table: %v", err)
	}
	if sandboxes := filepath.Join(app.Root, "sandboxes"); strings.Contains(string(mounts), sandboxes) {
		t.Errorf("the failed run left a mount under %s", sandboxes)
	}
}

func newRunApp(t *testing.T) (App, *bytes.Buffer) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("shard run needs root")
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
	t.Cleanup(func() { exec.Command("umount", "-l", filepath.Join(root, "runsc", "null-netns")).Run() })

	// No factory override: these two tests drive the real wiring, which is the whole point of them.
	return App{Version: "test", Root: root, Out: out, Err: out, Timeout: 5 * time.Minute}, out
}

// runApp builds a second view of the same layers, so the test can stop what the run left running.
func runApp(t *testing.T, app App, initPath string) runDeps {
	t.Helper()

	deps, err := defaultRunDeps(app, runOptions{initPath: initPath})
	if err != nil {
		t.Fatalf("build the run dependencies: %v", err)
	}

	return deps
}

// cleanUp ends the sandbox the test left running and gives its address back. It builds its own
// context, because the test's own is already cancelled by the time a cleanup runs.
func cleanUp(t *testing.T, deps runDeps, id string) {
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

// records reads the state tree the run wrote, rather than reaching back through the run's own repository.
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
		t.Fatalf("the run left %d records, want one", len(left))
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
