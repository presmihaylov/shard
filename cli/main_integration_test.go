//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// stopGrace is generous: the entrypoint is already gone by the time the cleanup stops the sandbox.
const stopGrace = 10 * time.Second

// waitBudget bounds a wait for something the daemon does on its own, after the call it answers.
const waitBudget = 30 * time.Second

// startBudget bounds the wait for the socket line of a daemon this run started.
const startBudget = 30 * time.Second

// shard is the binary this run built, and daemonUnderTest is the one daemon every test drives.
var (
	shard           string
	daemonUnderTest *testDaemon
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cli integration tests: %v\n", err)
	}

	os.Exit(code)
}

// run gives the package one daemon of its own, so no test speaks to the daemon of the systemd unit.
func run(m *testing.M) (int, error) {
	if !hostRunsSandboxes() {
		// Every test skips itself on such a host, and a daemon it cannot use would only fail to start.
		return m.Run(), nil
	}

	build, err := os.MkdirTemp("", "shard-build")
	if err != nil {
		return 1, fmt.Errorf("make a build directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(build); err != nil {
			fmt.Fprintf(os.Stderr, "remove %s: %v\n", build, err)
		}
	}()

	// The daemon under test is this tree, not whatever binary the box happens to have installed.
	shard = filepath.Join(build, "shard")
	out, err := exec.Command(goTool(), "build", "-o", shard, "github.com/presmihaylov/shard/cmd/shard").CombinedOutput()
	if err != nil {
		return 1, fmt.Errorf("build shard: %w: %s", err, out)
	}

	daemonUnderTest, err = spawnDaemon()
	if err != nil {
		return 1, err
	}

	code := m.Run()

	if err := daemonUnderTest.stop(); err != nil {
		return 1, err
	}

	return code, nil
}

// goTool is what the Makefile falls back to as well: sudo drops /usr/local/go/bin from PATH.
func goTool() string {
	if path, err := exec.LookPath("go"); err == nil {
		return path
	}

	return "/usr/local/go/bin/go"
}

// hostRunsSandboxes reports whether this host can run one at all. The tests skip themselves on it too.
func hostRunsSandboxes() bool {
	if os.Geteuid() != 0 {
		return false
	}

	for _, binary := range []string{"runsc", "ip", "nft"} {
		if _, err := exec.LookPath(binary); err != nil {
			return false
		}
	}

	_, err := os.Stat(hostInitPath)

	return err == nil
}

// testDaemon is a shard daemon this run started, over a root only it holds.
type testDaemon struct {
	root string
	cmd  *exec.Cmd
	log  string
}

// spawnDaemon runs the daemon over a fresh root and waits for the line that says its socket is up.
// env is added to the daemon's own environment, which is how a test gives it a different wiring.
func spawnDaemon(env ...string) (*testDaemon, error) {
	root, err := os.MkdirTemp("", "shard-itest")
	if err != nil {
		return nil, fmt.Errorf("make a state root: %w", err)
	}

	log, err := os.CreateTemp("", "shard-daemon")
	if err != nil {
		return nil, fmt.Errorf("make a daemon log: %w", err)
	}

	cmd := exec.Command(shard, "--root", root, "daemon")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start the daemon: %w", err), log.Close())
	}

	d := &testDaemon{root: root, cmd: cmd, log: log.Name()}
	if err := errors.Join(d.await(), log.Close()); err != nil {
		return nil, errors.Join(err, d.stop())
	}

	return d, nil
}

// await waits for the log line rather than for the file: the socket exists a moment before its mode is set.
func (d *testDaemon) await() error {
	deadline := time.Now().Add(startBudget)
	for {
		written, err := os.ReadFile(d.log)
		if err != nil {
			return fmt.Errorf("read %s: %w", d.log, err)
		}
		if strings.Contains(string(written), "api listening on") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon logged no socket in %s:\n%s", startBudget, written)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// stop ends the daemon by its own pid, proves its socket is gone, and gives the root back.
func (d *testDaemon) stop() error {
	if err := d.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal the daemon: %w", err)
	}
	if err := d.cmd.Wait(); err != nil {
		return fmt.Errorf("the daemon ended with %w:\n%s", err, d.logged())
	}

	socket := filepath.Join(d.root, api.SocketFile)
	if _, err := os.Lstat(socket); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the socket %s outlived the daemon: %w", socket, err)
	}

	// A create that failed leaves the runsc null-netns behind, and the removal below trips over it.
	if err := exec.Command("umount", "-l", filepath.Join(d.root, "runsc", "null-netns")).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "unmount the runsc null-netns of %s: %v\n", d.root, err)
	}

	return errors.Join(os.RemoveAll(d.root), os.Remove(d.log))
}

// logged is what the daemon wrote, which is the only account of a failure that happened inside it.
func (d *testDaemon) logged() string {
	written, err := os.ReadFile(d.log)
	if err != nil {
		return fmt.Sprintf("read %s: %v", d.log, err)
	}

	return string(written)
}

// newCreateApp answers a client of the package's daemon, and skips on a host that runs no sandbox.
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

	return appFor(daemonUnderTest.root)
}

// ownDaemon gives one test a second daemon, wired by env, over a root the package's daemon never holds.
func ownDaemon(t *testing.T, env ...string) (App, *bytes.Buffer) {
	t.Helper()

	d, err := spawnDaemon(env...)
	if err != nil {
		t.Fatalf("start a daemon of this test's own: %v", err)
	}
	t.Cleanup(func() {
		if err := d.stop(); err != nil {
			t.Errorf("stop the daemon of this test: %v", err)
		}
	})

	return appFor(d.root)
}

func appFor(root string) (App, *bytes.Buffer) {
	out := &bytes.Buffer{}

	return App{Version: "test", Root: root, Out: out, Err: out, Timeout: 5 * time.Minute}, out
}

// daemonClient reads what a verb left, over the same socket every verb speaks to.
func daemonClient(app App) *client.Client {
	c := client.New(app.Root)
	c.Timeout = waitBudget

	return c
}

// create runs the command under test and answers with the id it printed.
func create(t *testing.T, app App, out *bytes.Buffer, argv ...string) string {
	t.Helper()

	args := append([]string{"create", testImage, "--"}, argv...)
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}

	id := strings.TrimSpace(out.String())
	if id == "" {
		t.Fatal("create printed no id")
	}
	out.Reset()

	return id
}

// record is the sandbox as the daemon reports it, which is the only record a test reads.
func record(t *testing.T, app App, id string) models.Sandbox {
	t.Helper()

	sb, err := daemonClient(app).GetSandbox(t.Context(), id)
	if err != nil {
		t.Fatalf("inspect %s: %v", id, err)
	}

	return sb.Sandbox
}

// cleanUp ends the sandbox a test left running, over the socket, so nothing of it outlives the test.
func cleanUp(t *testing.T, app App, id string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := daemonClient(app).RemoveSandbox(ctx, id, true, stopGrace); err != nil {
		var missing *client.NotFoundError
		if !errors.As(err, &missing) {
			t.Logf("remove %s: %v", id, err)
		}
	}
}

// holdings names everything the host holds for a sandbox: its record, its lease and its runsc container.
func holdings(t *testing.T, app App) []string {
	t.Helper()

	list, err := daemonClient(app).ListSandboxes(t.Context(), true)
	if err != nil {
		t.Fatalf("list the sandboxes: %v", err)
	}

	held := make([]string, 0, len(list.Sandboxes))
	for _, sb := range list.Sandboxes {
		held = append(held, "record:"+sb.ID)
	}
	for _, id := range leases(t, app.Root) {
		held = append(held, "lease:"+id)
	}
	for _, id := range containers(t, app.Root) {
		held = append(held, "runsc:"+id)
	}
	slices.Sort(held)

	return held
}

// leases answers the id in every lease file, because the file itself is named after the address.
func leases(t *testing.T, root string) []string {
	t.Helper()

	dir := filepath.Join(root, "network", "leases")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the leases: %v", err)
	}

	held := make([]string, 0, len(entries))
	for _, entry := range entries {
		written, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read the lease %s: %v", entry.Name(), err)
		}

		held = append(held, strings.TrimSpace(string(written)))
	}

	return held
}

// containers is what runsc holds under root. null-netns is the bind mount runsc makes for itself on
// the first create, and it belongs to no sandbox.
func containers(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "runsc"))
	if errors.Is(err, fs.ErrNotExist) {
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

// alive reports whether the host still runs the process the record named. A zombie is not one: the
// sandbox is a child of the daemon, so its entry outlives it until the daemon reaps it.
func alive(t *testing.T, pid int) bool {
	t.Helper()

	if pid == 0 {
		return false
	}

	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("read the stat of %d: %v", pid, err)
	}

	// The state follows the command name, which is the one field that may itself hold a space.
	fields := strings.Fields(string(stat[strings.LastIndex(string(stat), ")")+1:]))
	if len(fields) == 0 {
		t.Fatalf("the stat of %d names no state: %q", pid, stat)
	}

	return fields[0] != "Z"
}

// awaitEntrypoint waits for the exit status the supervisor writes into the guest, which a root exec reads.
// It is bounded, because a supervisor that lost the right to write it left a wait that never returned.
func awaitEntrypoint(t *testing.T, app App, id string) models.ExitStatus {
	t.Helper()

	deadline := time.Now().Add(waitBudget)
	for {
		written, err := runExec(t, app, "exec", "--user", "root", id, "--", "/bin/cat", "/.shard/exit.json")
		if err == nil {
			var status models.ExitStatus
			if err := json.Unmarshal([]byte(written), &status); err != nil {
				t.Fatalf("the supervisor wrote the unreadable exit status %q: %v", written, err)
			}

			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the entrypoint of %s wrote no exit status in %s: %v", id, waitBudget, err)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// guestOutput is what the entrypoint wrote, over the same socket shard logs reads it from.
func guestOutput(t *testing.T, app App, id string) string {
	t.Helper()

	written := &bytes.Buffer{}
	if err := daemonClient(app).Logs(t.Context(), id, false, written); err != nil {
		t.Fatalf("read the logs of %s: %v", id, err)
	}

	return written.String()
}
