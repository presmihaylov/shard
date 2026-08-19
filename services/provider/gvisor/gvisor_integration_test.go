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
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// stopGrace is generous: these entrypoints are already gone, so nothing here waits it out.
const stopGrace = 10 * time.Second

func TestConformance(t *testing.T) {
	h := newHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider:    h.provider,
		NewSpec:     func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/true") },
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
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

// A sandbox outlives its entrypoint, so a wait that answered must not have ended it.
func TestTheSandboxIsStillAliveAfterTheEntrypointExits(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/true")

	if _, err := h.provider.Wait(t.Context(), spec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	alive, err := h.provider.Alive(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Alive: %v", err)
	}
	if !alive {
		t.Fatal("the sandbox died with its entrypoint, and only Stop may end one")
	}
}

// A stop signals first and kills only when the grace runs out, so it must not wait the grace out
// on a sandbox that has nothing left to refuse it.
func TestStopEndsASandboxWellInsideItsGrace(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/true")

	if _, err := h.provider.Wait(t.Context(), spec.ID); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	const grace = 60 * time.Second

	started := time.Now()
	if err := h.provider.Stop(t.Context(), spec.ID, grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A wide margin: this fails only when the TERM path does nothing and SIGKILL is what ends it.
	if elapsed := time.Since(started); elapsed > grace/3 {
		t.Errorf("Stop took %s of a %s grace, so SIGTERM did not end the sandbox", elapsed, grace)
	}
}

func TestAliveIsFalseForASandboxRunscNeverHeld(t *testing.T) {
	h := newHarness(t)

	alive, err := h.provider.Alive(t.Context(), "shard-12-never-created")
	if err != nil {
		t.Fatalf("Alive: %v", err)
	}
	if alive {
		t.Error("Alive reported a sandbox runsc does not hold")
	}
}

// harness holds what every sandbox in one test shares: the image, the runsc root and the id lookup.
type harness struct {
	provider *gvisor.Provider
	image    image.Image

	mu   sync.Mutex
	dirs map[string]string
	next atomic.Int64
}

func newHarness(t *testing.T) *harness {
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

	runscRoot := filepath.Join(t.TempDir(), "runsc")

	runner, err := runsc.New(runscRoot)
	if err != nil {
		t.Fatalf("open the runsc runner: %v", err)
	}
	// --network=none leaves a bind mount in the runsc root that the TempDir removal would trip over.
	t.Cleanup(func() { exec.Command("umount", "-l", filepath.Join(runscRoot, "null-netns")).Run() })

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

	h.mu.Lock()
	h.dirs[id] = dir
	h.mu.Unlock()

	t.Cleanup(func() {
		// Best effort: a subtest may have stopped and removed this one already, and its errors say nothing new.
		ctx := context.Background()
		h.provider.Stop(ctx, id, stopGrace)
		h.provider.Remove(ctx, id)
	})

	return models.SandboxSpec{
		ID:          id,
		StateDir:    dir,
		RootFS:      h.image.RootFS,
		ImageConfig: h.image.Config,
		Entrypoint:  entrypoint,
	}
}

func (h *harness) start(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	spec := h.newSpec(t, entrypoint...)

	runtime, err := h.provider.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if runtime.PID <= 0 {
		t.Errorf("Create reported pid %d, and the sandbox process has a real one", runtime.PID)
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

// imageRoot outlives every test, so TestMain owns it rather than any one t.TempDir.
var imageRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "shard-12-images")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create the image root:", err)
		os.Exit(1)
	}

	imageRoot = root
	code := m.Run()

	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, "remove the image root:", err)
	}

	os.Exit(code)
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
