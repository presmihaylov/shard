//go:build integration

package gvisor_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
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

// stopGrace is generous: these entrypoints are already gone, so nothing here waits it out.
const stopGrace = 10 * time.Second

func TestConformance(t *testing.T) {
	h := newHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/true") },
		NewIgnoresTermSpec: func(t *testing.T) models.SandboxSpec {
			// The marker comes after the trap, so the suite never stops an entrypoint that still dies on SIGTERM.
			script := fmt.Sprintf("trap '' TERM; echo %s; while true; do sleep 1; done", conformance.ReadyMarker)

			return h.newSpec(t, "/bin/sh", "-c", script)
		},
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
	})
}

// TestTheEntrypointExitCodePropagates is half the SHARD-12 acceptance criterion.
func TestTheEntrypointExitCodePropagates(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "exit 7")

	status, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if status.Code != 7 {
		t.Errorf("got exit code %d, want 7", status.Code)
	}
	if status.Signal != 0 {
		t.Errorf("got signal %v, want none: the entrypoint exited on its own", status.Signal)
	}
}

// A signalled entrypoint has no exit code of its own, so the record must carry the signal too.
func TestASignalledEntrypointReportsItsSignal(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "kill -9 $$")

	status, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if status.Signal != 9 {
		t.Errorf("got signal %v, want 9", status.Signal)
	}
	if status.Code != 137 {
		t.Errorf("got exit code %d, want 137, which is what a shell reports for SIGKILL", status.Code)
	}
}

// TestTheGuestOutputStreams is the other half: runsc create hands the fds over and exits, and the
// sandbox keeps writing to them.
func TestTheGuestOutputStreams(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "echo out-from-the-guest; echo err-from-the-guest >&2")

	if _, err := h.provider.Wait(t.Context(), spec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}

	got := readFile(t, path)
	for _, want := range []string{"out-from-the-guest", "err-from-the-guest"} {
		if !strings.Contains(got, want) {
			t.Errorf("the sandbox log holds %q, which is missing %q", got, want)
		}
	}
}

// A stop is final. Only Remove and a second Create re-run an entrypoint, which is what keeps the
// writable layer: runsc refuses to start a container it has already stopped.
func TestAStoppedSandboxStartsAgainOverWhatItKept(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "test -f /root/marker && echo seen-by-the-second-run; touch /root/marker")

	if _, err := h.provider.Wait(t.Context(), spec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("the second Start: %v", err)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("the second Wait: %v", err)
	}
	if exit.Code != 0 {
		t.Errorf("the second run exited %d, want 0", exit.Code)
	}

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if got := readFile(t, path); !strings.Contains(got, "seen-by-the-second-run") {
		t.Errorf("the second run read back %q, want the file the first run wrote", got)
	}
}

// runsc start only unblocks the task and reads nothing back, so a Start that returned nil used to
// mean nothing at all: the supervisor could already be dead over an entrypoint that does not exist.
func TestStartRefusesAnEntrypointThatNeverRan(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/no/such/entrypoint")

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := h.provider.Start(t.Context(), spec.ID)
	if err == nil {
		t.Fatal("Start reported success for an entrypoint the image does not hold")
	}
	// The message carries what the supervisor printed, because the caller drops the log with the rest.
	if !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), "/no/such/entrypoint") {
		t.Errorf("Start failed with %v, want it to name the entrypoint that did not start", err)
	}

	assertAlive(t, h, spec.ID, false)
}

// The merged view is the sandbox's rootfs, so nothing may remove the state directory while it stands.
func TestStopAndRemoveBothDropTheWritableLayerMount(t *testing.T) {
	h := newHarness(t)

	stopped := h.start(t, "/bin/true")
	assertMounted(t, h, stopped.ID, true)
	if err := h.provider.Stop(t.Context(), stopped.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertMounted(t, h, stopped.ID, false)

	// Remove takes the same sandbox down from running, because --force is what makes that safe.
	removed := h.start(t, "/bin/sh", "-c", "sleep 3600")
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
	first := h.start(t, "/bin/sh", "-c", "echo written-by-the-first-run > /root/marker")

	if _, err := h.provider.Wait(t.Context(), first.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := h.provider.Stop(t.Context(), first.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.provider.Remove(t.Context(), first.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	second := first
	second.Entrypoint = []string{"/bin/sh", "-c", "cat /root/marker"}

	if err := h.provider.Create(t.Context(), second); err != nil {
		t.Fatalf("the second Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), second.ID); err != nil {
		t.Fatalf("the second Start: %v", err)
	}
	if _, err := h.provider.Wait(t.Context(), second.ID); err != nil {
		t.Fatalf("the second Wait: %v", err)
	}

	path, err := h.provider.LogPath(second.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if got := readFile(t, path); !strings.Contains(got, "written-by-the-first-run") {
		t.Errorf("the second run read back %q, want what the first run wrote", got)
	}
}

// A second Create on a live id must refuse. Without the check its rollback would unmount the rootfs
// the first sandbox is running on.
func TestCreateRefusesAnIdThatIsAlreadyLive(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 3600")

	if err := h.provider.Create(t.Context(), spec); err == nil {
		t.Fatal("Create accepted an id that is already running")
	}

	assertAlive(t, h, spec.ID, true)
	assertMounted(t, h, spec.ID, true)
}

// Create must refuse an orphaned mount too: building over it would give two sandboxes one writable layer.
func TestCreateRefusesASandboxRunscLostThatIsStillMounted(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 3600")
	h.loseTheSandbox(t, spec.ID)

	if err := h.provider.Create(t.Context(), spec); err == nil {
		t.Fatal("Create built a second sandbox over a rootfs runsc no longer holds")
	}

	assertMounted(t, h, spec.ID, true)
}

// Hand-deleted runsc metadata over a live mount is the one case where an unmount drops a running
// sandbox's rootfs. Remove must refuse instead, and shard rm --force is SHARD-24's answer.
func TestRemoveRefusesWhenRunscLostASandboxThatIsStillMounted(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 3600")
	h.loseTheSandbox(t, spec.ID)

	if err := h.provider.Remove(t.Context(), spec.ID); err == nil {
		t.Fatal("Remove unmounted a sandbox runsc no longer knows about")
	}

	assertMounted(t, h, spec.ID, true)
}

// Stop reaches the same unmount as Remove, so it owes the same refusal over hand-deleted metadata.
func TestStopRefusesWhenRunscLostASandboxThatIsStillMounted(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 3600")
	h.loseTheSandbox(t, spec.ID)

	if err := h.provider.Stop(t.Context(), spec.ID, 5*time.Second); err == nil {
		t.Fatal("Stop unmounted a sandbox runsc no longer knows about")
	}

	assertMounted(t, h, spec.ID, true)
}

// A wait in flight when a stop lands must report the signal, and never a clean exit that never happened.
func TestAWaitInFlightSeesTheStopSignal(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "sleep 3600")

	answered := make(chan models.ExitStatus, 1)
	failed := make(chan error, 1)
	go func() {
		status, err := h.provider.Wait(context.Background(), spec.ID)
		if err != nil {
			failed <- err

			return
		}
		answered <- status
	}()

	// Long enough for the wait to be polling, and short enough that the entrypoint is still asleep.
	time.Sleep(500 * time.Millisecond)
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case status := <-answered:
		if status.Signal != int(syscall.SIGTERM) {
			t.Errorf("the wait reported signal %v, want SIGTERM: the stop is what ended the entrypoint", status.Signal)
		}
	case err := <-failed:
		t.Fatalf("the wait failed: %v", err)
	case <-time.After(stopGrace):
		t.Fatal("the wait never answered after the stop ended the sandbox")
	}
}

// One runsc root holds every sandbox, so a verb on one must not reach any other.
func TestOneSandboxIsUnmovedByAnother(t *testing.T) {
	h := newHarness(t)

	long := h.start(t, "/bin/sh", "-c", "sleep 3600")
	short := h.start(t, "/bin/sh", "-c", "exit 9")

	status, err := h.provider.Wait(t.Context(), short.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status.Code != 9 {
		t.Errorf("got exit code %d, want 9", status.Code)
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

	mu   sync.Mutex
	dirs map[string]string
	next atomic.Int64
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

	// Stop and Remove now refuse this mount, so only the test can drop it before the TempDir removal.
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
func (h *harness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
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
		ID:         id,
		StateDir:   dir,
		RootFS:     h.image.RootFS,
		Entrypoint: entrypoint,
		Network:    networkSpec,
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

func (h *harness) start(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t, entrypoint...)

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	return spec
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

	sweepCgroups()

	for _, target := range roots {
		if err := os.RemoveAll(*target); err != nil {
			fmt.Fprintln(os.Stderr, "remove", *target, err)
		}
	}

	os.Exit(code)
}

// sweepCgroups removes what a failed create leaves at the cgroup root. runsc removes its own cgroup
// on delete, but one left behind silently unbounds the next sandbox that takes the same id, and the
// ids here repeat on every run.
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
