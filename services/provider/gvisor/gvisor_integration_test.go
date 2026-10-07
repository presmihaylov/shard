//go:build integration

package gvisor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/hostclean"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/runspec"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// stopGrace is generous: shard-init exits on TERM, so nothing here waits it out.
const stopGrace = 10 * time.Second

// waitGrace bounds the wait on a process that ends by itself, so one that hangs never hangs the run.
const waitGrace = 30 * time.Second

func TestConformance(t *testing.T) {
	h := newHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider:  h.provider,
		NewSpec:   h.newSpec,
		EmptyDir:  func(t *testing.T) string { return t.TempDir() },
		Shell:     func(script string) []string { return []string{"/bin/sh", "-c", script} },
		Reopen:    h.reopen,
		HostLayer: true,
	})
}

// TestLaunch proves an exec on gVisor answers only once its command's execve took (SHARD-497).
func TestLaunch(t *testing.T) {
	h := newHarness(t)

	conformance.RunLaunch(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  h.newSpec,
		Shell:    func(script string) []string { return []string{"/bin/sh", "-c", script} },
	})
}

// TestPorts proves a forward on gVisor reaches a listener on the sandbox's own loopback (SHARD-789); runsc forwards only into a netstack with a network, so the sandbox gets one.
func TestPorts(t *testing.T) {
	h := newNetworkedHarness(t)

	conformance.RunPorts(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  h.newSpec,
		Shell:    func(script string) []string { return []string{"/bin/sh", "-c", script} },
	})
}

// TestAProcessExitCodePropagates is half the SHARD-12 acceptance criterion.
func TestAProcessExitCodePropagates(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)

	report, _ := h.runToEnd(t, spec.ID, "exit", "/bin/sh", "-c", "exit 7")
	if report.Exit == nil || report.Exit.Code != 7 || report.Exit.Signal != 0 {
		t.Errorf("the process ended %+v, want exit code 7 and no signal", report.Exit)
	}
}

// A signalled process has no exit code of its own, so the report must carry the signal too.
func TestASignalledProcessReportsItsSignal(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)

	report, _ := h.runToEnd(t, spec.ID, "killed", "/bin/sh", "-c", "kill -9 $$")
	if report.Exit == nil || report.Exit.Signal != 9 || report.Exit.Code != 137 {
		t.Errorf("the process ended %+v, want signal 9 and the 137 a shell reports for it", report.Exit)
	}
}

// TestTheGuestOutputStreams is the other half: shard-init points a process's stdout and stderr at its log on the sandbox's disk.
func TestTheGuestOutputStreams(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)

	_, got := h.runToEnd(t, spec.ID, "talker", "/bin/sh", "-c", "echo out-from-the-guest; echo err-from-the-guest >&2")
	for _, want := range []string{"out-from-the-guest", "err-from-the-guest"} {
		if !strings.Contains(got, want) {
			t.Errorf("the process log holds %q, which is missing %q", got, want)
		}
	}
}

// runsc refuses to start a container it has already stopped, so a start again re-creates it over the writable layer it kept.
func TestAStoppedSandboxStartsAgainOverWhatItKept(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)
	marker := "test -f /root/marker && echo seen-by-the-second-run; touch /root/marker"

	h.runToEnd(t, spec.ID, "first", "/bin/sh", "-c", marker)
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("the second Start: %v", err)
	}
	report, got := h.runToEnd(t, spec.ID, "second", "/bin/sh", "-c", marker)
	if report.Exit == nil || report.Exit.Code != 0 {
		t.Errorf("the second run ended %+v, want 0", report.Exit)
	}
	if !strings.Contains(got, "seen-by-the-second-run") {
		t.Errorf("the second run read back %q, want the file the first run wrote", got)
	}
}

// shard-init answers a start only once the execve took, so a command the image does not hold reaches the caller and the sandbox runs on.
func TestStartProcessRefusesACommandThatNeverRan(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)

	err := h.provider.StartProcess(t.Context(), spec.ID, models.ProcessSpec{Name: "missing", Argv: []string{"/no/such/command"}})
	var refused *models.CommandNotStartedError
	if !errors.As(err, &refused) {
		t.Fatalf("StartProcess failed with %v, want CommandNotStartedError", err)
	}
	if refused.Code != models.CommandNotFoundExitCode {
		t.Errorf("StartProcess returned exit code %d, want %d", refused.Code, models.CommandNotFoundExitCode)
	}

	assertAlive(t, h, spec.ID, true)
}

// The merged view is the sandbox's rootfs, so nothing may remove the state directory while it stands.
func TestStopAndRemoveBothDropTheWritableLayerMount(t *testing.T) {
	h := newHarness(t)

	stopped := h.start(t)
	assertMounted(t, h, stopped.ID, true)
	if err := h.provider.Stop(t.Context(), stopped.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertMounted(t, h, stopped.ID, false)

	// Remove takes the same sandbox down from running, because --force is what makes that safe.
	removed := h.start(t)
	assertMounted(t, h, removed.ID, true)
	if err := h.provider.Remove(t.Context(), removed.ID); err != nil {
		t.Fatalf("Remove a running sandbox: %v", err)
	}
	assertAlive(t, h, removed.ID, false)
	assertMounted(t, h, removed.ID, false)
}

// This is the restart story: a stop keeps the upper layer, and a second create over the same state
// directory reads it back.
func TestASecondCreateReadsBackWhatTheFirstRunWrote(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)

	h.runToEnd(t, spec.ID, "writer", "/bin/sh", "-c", "echo written-by-the-first-run > /root/marker")
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("the second Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("the second Start: %v", err)
	}
	if _, got := h.runToEnd(t, spec.ID, "reader", "/bin/sh", "-c", "cat /root/marker"); !strings.Contains(got, "written-by-the-first-run") {
		t.Errorf("the second run read back %q, want what the first run wrote", got)
	}
}

// A second Create on a live id must refuse. Without the check its rollback would unmount the rootfs
// the first sandbox is running on.
func TestCreateRefusesAnIdThatIsAlreadyLive(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)

	if err := h.provider.Create(t.Context(), spec); err == nil {
		t.Fatal("Create accepted an id that is already running")
	}

	assertAlive(t, h, spec.ID, true)
	assertMounted(t, h, spec.ID, true)
}

// Create must refuse an orphaned mount too: building over it would give two sandboxes one writable layer.
func TestCreateRefusesASandboxRunscLostThatIsStillMounted(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)
	h.loseTheSandbox(t, spec.ID)

	if err := h.provider.Create(t.Context(), spec); err == nil {
		t.Fatal("Create built a second sandbox over a rootfs runsc no longer holds")
	}

	assertMounted(t, h, spec.ID, true)
}

// The cgroup proves the rootfs is idle even when the runtime metadata is gone.
func TestRemoveDropsTheMountAfterRunscLostASandbox(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)
	h.loseTheSandbox(t, spec.ID)

	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove after the runtime state went: %v", err)
	}

	assertMounted(t, h, spec.ID, false)
}

// Stop uses the runtime status, so absent metadata cannot prove that the rootfs is idle.
func TestStopRefusesWhenRunscLostASandboxThatIsStillMounted(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)
	h.loseTheSandbox(t, spec.ID)

	if err := h.provider.Stop(t.Context(), spec.ID, 5*time.Second); err == nil {
		t.Fatal("Stop unmounted a sandbox runsc no longer knows about")
	}

	assertMounted(t, h, spec.ID, true)
}

// A process stop must report the TERM that ended it, and never a clean exit that never happened.
func TestAStoppedProcessReportsTheTerm(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t)
	h.run(t, spec.ID, "sleeper", "sleep", "3600")

	if err := h.provider.StopProcess(t.Context(), spec.ID, "sleeper", stopGrace); err != nil {
		t.Fatalf("StopProcess: %v", err)
	}

	report := h.awaitEnded(t, spec.ID, "sleeper")
	if report.State != models.ProcessKilled || report.Exit == nil || report.Exit.Signal != int(syscall.SIGTERM) {
		t.Errorf("the stopped process reads %+v, want killed by SIGTERM", report)
	}
}

// One runsc root holds every sandbox, so a verb on one must not reach any other.
func TestOneSandboxIsUnmovedByAnother(t *testing.T) {
	h := newHarness(t)

	long := h.start(t)
	h.run(t, long.ID, "sleeper", "sleep", "3600")
	short := h.start(t)

	if report, _ := h.runToEnd(t, short.ID, "exit", "/bin/sh", "-c", "exit 9"); report.Exit == nil || report.Exit.Code != 9 {
		t.Errorf("the process ended %+v, want exit code 9", report.Exit)
	}

	if err := h.provider.Stop(t.Context(), short.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	assertAlive(t, h, long.ID, true)
	assertMounted(t, h, long.ID, true)
}

func assertAlive(t *testing.T, h *harness, id string, want bool) {
	t.Helper()

	status, err := h.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Alive() != want {
		t.Errorf("sandbox %s reports alive=%v, want %v", id, status.Alive(), want)
	}
}

// assertMounted reads the host's own mount table, because that is what a state directory removal trips over.
func assertMounted(t *testing.T, h *harness, id string, want bool) {
	t.Helper()

	dir, err := h.stateDir(id)
	if err != nil {
		t.Fatalf("state directory of %s: %v", id, err)
	}

	blob, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("read the mount table: %v", err)
	}

	got := strings.Contains(string(blob), filepath.Join(dir, "bundle", "rootfs"))
	if got != want {
		t.Errorf("the rootfs of %s is mounted=%v, want %v", id, got, want)
	}
}

// harness holds what every sandbox in one test shares: the image, the runsc root and the id lookup.
type harness struct {
	provider *gvisor.Provider
	image    image.Image
	// runscRoot is what the tests that go behind the provider's back need, and nothing else.
	runscRoot string
	// net is nil unless the harness is networked, and then every spec gets an allocated namespace.
	net *network.Service

	open func() (models.Provider, error)

	mu   sync.Mutex
	dirs map[string]string
	next atomic.Int64
}

func (h *harness) reopen(t *testing.T) models.Provider {
	t.Helper()

	p, err := h.open()
	if err != nil {
		t.Fatalf("open the provider again: %v", err)
	}

	return p
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, runsc.NetworkNone) }

// newNetworkedHarness gives every sandbox a namespace, an address and a route out. gVisor builds its
// netstack from the interfaces it finds at create, so the namespace is addressed before it joins one.
func newNetworkedHarness(t *testing.T) *harness {
	t.Helper()
	requireNetworkTools(t)

	return newHarnessWith(t, runsc.NetworkSandbox)
}

func newHarnessWith(t *testing.T, mode string) *harness {
	t.Helper()
	requireRunsc(t)

	if _, err := os.Stat(hostInitPath); err != nil {
		t.Skipf("no supervisor at %s: run make devbox-sync first", hostInitPath)
	}

	h := &harness{dirs: map[string]string{}}

	img, err := pullTestImage()
	if err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}
	h.image = img

	h.runscRoot = filepath.Join(t.TempDir(), "runsc")

	runner, err := runsc.New(h.runscRoot, runsc.WithNetwork(mode))
	if err != nil {
		t.Fatalf("open the runsc runner: %v", err)
	}

	if mode == runsc.NetworkSandbox {
		h.net = newNetworkService(t)
	}
	// --network=none leaves a bind mount in the runsc root that the TempDir removal would trip over.
	t.Cleanup(func() { exec.Command("umount", "-l", filepath.Join(h.runscRoot, "null-netns")).Run() })

	bundles, err := bundle.New(hostInitPath)
	if err != nil {
		t.Fatalf("open the bundle service: %v", err)
	}

	h.provider, err = gvisor.New(runner, bundles, h.stateDir)
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}
	// A daemon restart is a second provider over the same runner and state, which holds nothing of the first in memory.
	h.open = func() (models.Provider, error) { return gvisor.New(runner, bundles, h.stateDir) }

	return h
}

// loseTheSandbox deletes runsc's metadata behind the provider's back, which is the one state where an
// unmount would drop the rootfs of a sandbox that may still run.
func (h *harness) loseTheSandbox(t *testing.T, id string) {
	t.Helper()

	if err := h.runsc(t, "delete", "--force", id).Run(); err != nil {
		t.Fatalf("delete the runsc metadata by hand: %v", err)
	}

	dir, err := h.stateDir(id)
	if err != nil {
		t.Fatalf("the state directory of %s: %v", id, err)
	}

	// An orphaned mount can survive a refusal, so the test owns its cleanup.
	t.Cleanup(func() { exec.Command("umount", "-l", filepath.Join(dir, "bundle", "rootfs")).Run() })
}

// runsc drives the binary directly, which is how a test forges the state the provider must refuse.
func (h *harness) runsc(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()

	return exec.Command("runsc", append([]string{"--root", h.runscRoot}, args...)...)
}

func (h *harness) stateDir(id string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	dir, ok := h.dirs[id]
	if !ok {
		return "", fmt.Errorf("the test never made a sandbox called %s", id)
	}

	return dir, nil
}

// newSpec gives every sandbox its own id and its own state directory, and ends it when the test does.
func (h *harness) newSpec(t *testing.T) models.SandboxSpec {
	t.Helper()

	id := fmt.Sprintf("shard-12-%d", h.next.Add(1))
	dir := t.TempDir()

	networkSpec := h.allocate(t, id)

	h.mu.Lock()
	h.dirs[id] = dir
	h.mu.Unlock()

	t.Cleanup(func() {
		// Best effort: a subtest may have stopped and removed this one already, and its errors say nothing new.
		ctx := context.Background()
		h.provider.Stop(ctx, id, stopGrace)
		h.provider.Remove(ctx, id)
	})

	spec := models.SandboxSpec{
		ID:       id,
		StateDir: dir,
		RootFS:   h.image.RootFS,
		Network:  networkSpec,
	}

	return runspec.Resolve(spec, h.image.Config)
}

// allocate is a no-op on a harness with no network, which is every test outside the SHARD-13 file.
func (h *harness) allocate(t *testing.T, id string) models.NetworkSpec {
	t.Helper()

	if h.net == nil {
		return models.NetworkSpec{}
	}

	spec, err := h.net.Allocate(t.Context(), id)
	if err != nil {
		t.Fatalf("allocate the network of %s: %v", id, err)
	}
	t.Cleanup(func() {
		if err := h.net.Release(context.Background(), id); err != nil {
			t.Logf("release the network of %s: %v", id, err)
		}
	})

	return spec
}

// newNetworkService shares one lease directory across the package, so no two sandboxes in one run
// claim the same address and therefore the same host interface name.
func newNetworkService(t *testing.T) *network.Service {
	t.Helper()

	m, err := netns.New()
	if err != nil {
		t.Fatalf("open the netns manager: %v", err)
	}

	s, err := network.New(network.Config{Root: networkRoot}, m)
	if err != nil {
		t.Fatalf("open the network service: %v", err)
	}

	return s
}

func requireNetworkTools(t *testing.T) {
	t.Helper()

	for _, binary := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("no %s on this host", binary)
		}
	}
}

// start creates and starts a sandbox, which runs shard-init alone until a test starts a process in it.
func (h *harness) start(t *testing.T) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t)

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return spec
}

// run starts one named process in a started sandbox.
func (h *harness) run(t *testing.T, id, name string, argv ...string) {
	t.Helper()

	if err := h.provider.StartProcess(t.Context(), id, models.ProcessSpec{Name: name, Argv: argv}); err != nil {
		t.Fatalf("StartProcess %s: %v", name, err)
	}
}

// runToEnd runs one process and returns its last report and its log once it ended.
func (h *harness) runToEnd(t *testing.T, id, name string, argv ...string) (models.ProcessReport, string) {
	t.Helper()

	h.run(t, id, name, argv...)
	report := h.awaitEnded(t, id, name)

	return report, h.logOf(t, id, name)
}

// awaitEnded polls the table, because shard-init reports an exit after it reaped the process.
func (h *harness) awaitEnded(t *testing.T, id, name string) models.ProcessReport {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), waitGrace)
	defer cancel()
	for {
		rows, err := h.provider.Processes(ctx, id)
		if err != nil {
			t.Fatalf("Processes: %v", err)
		}
		for _, row := range rows {
			if row.Name == name && row.State.Ended() {
				return row
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not end within %s: %+v", name, waitGrace, rows)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// logOf reads what a process wrote so far.
func (h *harness) logOf(t *testing.T, id, name string) string {
	t.Helper()

	path, err := h.provider.ProcessLogPath(id, name)
	if err != nil {
		t.Fatalf("ProcessLogPath %s: %v", name, err)
	}

	return readFile(t, path)
}

// The image is read-only and shared by design, so every test in this package pulls it once.
var pullTestImage = sync.OnceValues(func() (image.Image, error) {
	svc, err := image.New(imageRoot)
	if err != nil {
		return image.Image{}, fmt.Errorf("open the image service: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	return svc.Pull(ctx, testImage)
})

// imageRoot and networkRoot outlive every test, so TestMain owns them rather than any one t.TempDir.
var (
	imageRoot   string
	networkRoot string
)

func TestMain(m *testing.M) {
	if err := hostclean.Refuse(stateRoots()...); err != nil {
		fmt.Fprintln(os.Stderr, "gvisor integration tests:", err)
		os.Exit(1)
	}
	teardownOnSignal()

	roots := map[string]*string{"shard-12-images": &imageRoot, "shard-13-network": &networkRoot}

	for prefix, target := range roots {
		root, err := os.MkdirTemp("", prefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create the", prefix, "root:", err)
			os.Exit(1)
		}
		*target = root
	}

	code := m.Run()

	if err := teardown(); err != nil {
		fmt.Fprintln(os.Stderr, "give the host back:", err)

		code = 1
	}

	os.Exit(code)
}

// stateRoots are the roots this package makes. One left behind means an earlier run kept host state,
// so a run refuses to start on it.
func stateRoots() []string { return underTemp("shard-12-images", "shard-13-network") }

// tempPrefixes adds the scratch directory of an exec, which a killed run leaves and nothing pins.
func tempPrefixes() []string { return append(stateRoots(), underTemp("shard-exec-")...) }

func underTemp(prefixes ...string) []string {
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		out = append(out, filepath.Join(os.TempDir(), prefix))
	}

	return out
}

var torndown sync.Once

// teardown gives the host back: the cgroups a failed create left, then the mounts, the namespaces,
// the links and the roots. Every step runs even when one fails, or the next run trips over the rest.
func teardown() error {
	var err error
	torndown.Do(func() {
		sweepCgroups()
		err = hostclean.Sweep(tempPrefixes()...)
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

// sweepCgroups removes what a failed create leaves at the cgroup root, because a leftover cgroup silently unbounds the next sandbox that takes the same id, and the ids here repeat on every run.
func sweepCgroups() {
	left, err := filepath.Glob(filepath.Join(cgroup.Root, bundle.CgroupsPath("shard-1?-*")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "list the cgroups the tests left:", err)

		return
	}

	for _, dir := range left {
		if err := os.Remove(dir); err != nil {
			fmt.Fprintln(os.Stderr, "remove the leftover cgroup", dir+":", err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(blob)
}

func requireRunsc(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("runsc needs root")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("no runsc on this host")
	}
}
