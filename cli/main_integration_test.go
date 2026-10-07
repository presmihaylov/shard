//go:build integration

package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/hostclean"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/sandbox"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

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
	code, err := isolated(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cli integration tests: %v\n", err)
	}

	os.Exit(code)
}

// isolated is run under a saved connection of its own, which the shard binary under test inherits, so no run reads the user's.
func isolated(m *testing.M) (int, error) {
	cleanup, err := isolateConfig()
	if err != nil {
		return 1, err
	}

	code, err := run(m)
	if cleanupErr := cleanup(); cleanupErr != nil {
		return 1, errors.Join(err, cleanupErr)
	}

	return code, err
}

// run gives the package one daemon of its own, so no test speaks to the daemon of the systemd unit.
func run(m *testing.M) (int, error) {
	if !hostRunsSandboxes() {
		// Every test skips itself on such a host, and a daemon it cannot use would only fail to start.
		return m.Run(), nil
	}
	if err := hostclean.Refuse(stateRoots()...); err != nil {
		return 1, err
	}
	teardownOnSignal()

	build, err := os.MkdirTemp("", "shard-build")
	if err != nil {
		return 1, fmt.Errorf("make a build directory: %w", err)
	}

	// The daemon under test is this tree, not whatever binary the box happens to have installed.
	shard = filepath.Join(build, "shard")
	out, err := exec.Command(goTool(), "build", "-o", shard, "github.com/presmihaylov/shard/cmd/shard").CombinedOutput()
	if err != nil {
		return 1, errors.Join(fmt.Errorf("build shard: %w: %s", err, out), teardown())
	}

	daemonUnderTest, err = spawnDaemon()
	if err != nil {
		return 1, errors.Join(err, teardown())
	}

	code := m.Run()

	return code, teardown()
}

// stateRoots are the roots this package makes. One left behind means an earlier run kept host state,
// so a run refuses to start on it.
func stateRoots() []string {
	roots := append(underTemp(itestPrefix, "shard-build", "shard-daemon"), filepath.Join(shortTemp, itestPrefix))
	// Where $TMPDIR is /tmp the two itest prefixes are one, and a root matched twice is swept twice.
	slices.Sort(roots)

	return slices.Compact(roots)
}

// tempPrefixes adds the scratch directory of an exec, which a killed daemon leaves and nothing pins.
func tempPrefixes() []string { return append(stateRoots(), underTemp("shard-exec-")...) }

func underTemp(prefixes ...string) []string {
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		out = append(out, filepath.Join(os.TempDir(), prefix))
	}

	return out
}

var torndown sync.Once

// teardown gives the host back, and runs every step even when one fails, because a skipped step
// leaves a netns, a mount or an address lease that the next run trips over.
func teardown() error {
	var err error
	torndown.Do(func() {
		var errs []error
		// The sandboxes go first: only the daemon that holds the record can free what the record names.
		if daemonUnderTest != nil {
			errs = append(errs, removeEverySandbox(daemonUnderTest.root), daemonUnderTest.stop())
		}
		errs = append(errs, hostclean.Sweep(tempPrefixes()...))
		err = errors.Join(errs...)
	})

	return err
}

// teardownOnSignal gives the host back when the run is interrupted, which is how a developer ends a
// test that hangs. 130 is what a shell reports for a run that a SIGINT ended.
func teardownOnSignal() {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		signalled := <-interrupted
		if err := teardown(); err != nil {
			fmt.Fprintf(os.Stderr, "give the host back after %s: %v\n", signalled, err)
		}

		os.Exit(130)
	}()
}

// removeEverySandbox takes back what a test left, whether it passed, failed or never got to its own cleanup.
func removeEverySandbox(root string) error {
	c := client.New(root)
	c.Timeout = waitBudget

	ctx, cancel := context.WithTimeout(context.Background(), waitBudget)
	defer cancel()

	listed, err := c.ListSandboxes(ctx, true)
	if err != nil {
		return fmt.Errorf("list the sandboxes the tests left: %w", err)
	}

	var errs []error
	for _, left := range listed.Sandboxes {
		if err := c.RemoveSandbox(ctx, left.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("remove the sandbox %s the tests left: %w", left.ID, err))
		}
	}

	return errors.Join(errs...)
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

// itestProvider names the substrate the suite's daemon runs, so a /dev/kvm host does not auto-pick firecracker and leak a vmm; SHARD_ITEST_PROVIDER picks another for a box run.
var itestProvider = cmp.Or(os.Getenv("SHARD_ITEST_PROVIDER"), "gvisor")

// itestResources is the bound each create of the suite carries: firecracker refuses an unbounded guest, so a run there takes the floor it boots under.
func itestResources() sandbox.ResourceRequest {
	if itestProvider != firecracker.Name {
		return sandbox.ResourceRequest{}
	}

	return sandbox.ResourceRequest{MemoryMiB: new(int64(firecracker.MinMemoryMiB))}
}

// createArgs is the create verb over args, with the suite's bound unless args name their own.
func createArgs(args ...string) []string {
	return bounded("create", args)
}

func bounded(verb string, args []string) []string {
	bound := itestResources().MemoryMiB
	if bound == nil || slices.Contains(args, "--memory") {
		return append([]string{verb}, args...)
	}

	return append([]string{verb, "--memory", strconv.FormatInt(*bound, 10) + "MiB"}, args...)
}

// spawnDaemon runs the daemon over a fresh root and waits for the line that says its socket is up.
// env is added to the daemon's own environment, which is how a test gives it a different wiring.
func spawnDaemon(env ...string) (*testDaemon, error) {
	root, err := os.MkdirTemp("", itestPrefix)
	if err != nil {
		return nil, fmt.Errorf("make a state root: %w", err)
	}

	return daemonOver(root, env...)
}

// daemonOver runs the daemon over root, which may hold the records of a daemon that served it before.
func daemonOver(root string, env ...string) (*testDaemon, error) {
	log, err := os.CreateTemp("", "shard-daemon")
	if err != nil {
		return nil, fmt.Errorf("make a daemon log: %w", err)
	}

	cmd := exec.Command(shard, "--root", root, "daemon", "--provider", itestProvider)
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

// stop halts the daemon and gives the root back, every step even after a failure, because a leftover mount blocks the next run.
func (d *testDaemon) stop() error {
	// RemoveAll takes the records, the only handle on what a remove missed, and trips over a mount a failed create left.
	return errors.Join(d.halt(), hostclean.Release(d.root), os.RemoveAll(d.root))
}

// halt ends the daemon by its own pid and proves its socket is gone. The root and its sandboxes stay, for daemonOver.
func (d *testDaemon) halt() error {
	var errs []error
	if err := d.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		errs = append(errs, fmt.Errorf("signal the daemon: %w", err))
	}
	if err := d.cmd.Wait(); err != nil {
		errs = append(errs, fmt.Errorf("the daemon ended with %w:\n%s", err, d.logged()))
	}

	socket := filepath.Join(d.root, api.SocketFile)
	if _, err := os.Lstat(socket); !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("the socket %s outlived the daemon: %w", socket, err))
	}

	return errors.Join(append(errs, os.Remove(d.log))...)
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

// ownDaemon gives one test a daemon wired by env on its own root, and halts the package's daemon for it because a host runs one (SHARD-777).
func ownDaemon(t *testing.T, env ...string) (App, *bytes.Buffer) {
	t.Helper()

	if err := daemonUnderTest.halt(); err != nil {
		t.Fatalf("halt the package's daemon: %v", err)
	}
	// Registered first, so it runs after the stop of this test's daemon has given the host back.
	t.Cleanup(func() {
		resumed, err := daemonOver(daemonUnderTest.root)
		if err != nil {
			t.Errorf("serve the package's root again: %v", err)
			return
		}
		daemonUnderTest = resumed
	})

	d, err := spawnDaemon(env...)
	if err != nil {
		t.Fatalf("start a daemon of this test's own: %v", err)
	}
	t.Cleanup(func() {
		// The sandboxes go first: only the daemon that holds the record can free what the record names.
		if err := errors.Join(removeEverySandbox(d.root), d.stop()); err != nil {
			t.Errorf("stop the daemon of this test: %v", err)
		}
	})

	return appFor(d.root)
}

func appFor(root string) (App, *bytes.Buffer) {
	out := &bytes.Buffer{}

	return App{Version: "test", Root: root, Out: out, Err: out}, out
}

// daemonClient reads what a verb left, over the same socket every verb speaks to.
func daemonClient(app App) *client.Client {
	c := client.New(app.Root)
	c.Timeout = waitBudget

	return c
}

// processName is what every helper names the one process it runs, so a test reads it back by that name.
const processName = "app"

// runDetached creates a sandbox, runs argv in it once as the process app, and answers with the sandbox id.
func runDetached(t *testing.T, app App, out *bytes.Buffer, argv ...string) string {
	t.Helper()

	return runDetachedWith(t, app, out, nil, []string{"--restart", "no"}, argv...)
}

// runDetachedWith is runDetached with create flags for the sandbox and run flags for the process.
func runDetachedWith(t *testing.T, app App, out *bytes.Buffer, create, run []string, argv ...string) string {
	t.Helper()

	id := printedID(t, app, out, createArgs(append(slices.Clone(create), testImage)...))
	runIn(t, app, out, id, run, argv...)

	return id
}

// runIn runs argv as the process app of a running sandbox, under the run flags given.
func runIn(t *testing.T, app App, out *bytes.Buffer, id string, flags []string, argv ...string) {
	t.Helper()

	args := append(append([]string{"run", id, "--name", processName}, flags...), "--")
	app, stderr := ownStderr(app)
	if err := app.Run(t.Context(), append(args, argv...)); err != nil {
		t.Fatalf("run %v in %s: %v\n%s", argv, id, err, stderr)
	}
	if got := strings.TrimSpace(out.String()); got != processName {
		t.Fatalf("run printed %q, want the process name %s", got, processName)
	}
	out.Reset()
}

// printedID runs one verb that creates a sandbox; the pull progress goes to stderr, so the id is stdout alone.
func printedID(t *testing.T, app App, out *bytes.Buffer, argv []string) string {
	t.Helper()

	app, progress := ownStderr(app)
	if err := app.Run(t.Context(), argv); err != nil {
		t.Fatalf("%s: %v\n%s", argv[0], err, progress)
	}

	id := strings.TrimSpace(out.String())
	if id == "" {
		t.Fatalf("%s printed no id", argv[0])
	}
	out.Reset()

	return id
}

// ownStderr answers app with stderr on a buffer of its own, so what a verb prints there never lands in out.
func ownStderr(app App) (App, *bytes.Buffer) {
	stderr := &bytes.Buffer{}
	app.Err = stderr

	return app, stderr
}

// record is the sandbox as the daemon stored it, host side included, which no route answers.
func record(t *testing.T, app App, id string) models.Sandbox {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(app.Root, "sandboxes", id, "sandbox.json"))
	if err != nil {
		t.Fatalf("read the record of %s: %v", id, err)
	}

	var sb models.Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		t.Fatalf("decode the record of %s: %v", id, err)
	}

	return sb
}

// cleanUp ends the sandbox a test left running, over the socket, so nothing of it outlives the test.
func cleanUp(t *testing.T, app App, id string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := daemonClient(app).RemoveSandbox(ctx, id, true); err != nil {
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

// awaitProcess waits for the policy to end the process app, and answers its last exit.
// It is bounded, because a supervisor that lost the right to report it left a wait that never returned.
func awaitProcess(t *testing.T, app App, id string) models.ExitStatus {
	t.Helper()

	deadline := time.Now().Add(waitBudget)
	for {
		p, err := daemonClient(app).Process(t.Context(), id, processName)
		if err != nil {
			t.Fatalf("read process %s of %s: %v", processName, id, err)
		}
		if p.Status.State.Ended() && p.Status.Exit == nil {
			t.Fatalf("process %s of %s is %s and left no exit status", processName, id, p.Status.State)
		}
		if p.Status.State.Ended() {
			return *p.Status.Exit
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %s of %s is still %s after %s", processName, id, p.Status.State, waitBudget)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// processOf is the entry the record holds for the process app.
func processOf(t *testing.T, sb models.Sandbox) models.Process {
	t.Helper()

	i := slices.IndexFunc(sb.Processes, func(p models.Process) bool { return p.Name == processName })
	if i < 0 {
		t.Fatalf("the record of %s holds no process %s: %+v", sb.ID, processName, sb.Processes)
	}

	return sb.Processes[i]
}

// guestOutput is what the process app wrote, over the same socket shard logs reads it from.
func guestOutput(t *testing.T, app App, id string) string {
	t.Helper()

	written := &bytes.Buffer{}
	if err := daemonClient(app).Logs(t.Context(), id, processName, false, written); err != nil {
		t.Fatalf("read the logs of %s: %v", id, err)
	}

	return written.String()
}
