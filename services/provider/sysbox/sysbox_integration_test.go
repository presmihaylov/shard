//go:build integration

package sysbox_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/hostclean"
	"github.com/presmihaylov/shard/pkg/sysboxrunc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/sysbox"
	"github.com/presmihaylov/shard/services/runspec"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// stopGrace is generous: these entrypoints are already gone, so nothing here waits it out.
const stopGrace = 10 * time.Second

// TestConformance is the SHARD-93 checkpoint: the same suite gVisor passes, over sysbox-runc. Every
// snapshot verb is refused here, so the suite proves the refuse path and skips the rest.
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

type harness struct {
	provider *sysbox.Provider
	image    image.Image

	mu   sync.Mutex
	dirs map[string]string
	next atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireSysboxRunc(t)

	if _, err := os.Stat(hostInitPath); err != nil {
		t.Skipf("no supervisor at %s: run make devbox-sync first", hostInitPath)
	}

	h := &harness{dirs: map[string]string{}}

	img, err := pullTestImage()
	if err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}
	h.image = img

	runner, err := sysboxrunc.New(filepath.Join(t.TempDir(), "sysbox-runc"))
	if err != nil {
		t.Fatalf("open the sysbox-runc runner: %v", err)
	}

	bundles, err := bundle.New(hostInitPath)
	if err != nil {
		t.Fatalf("open the bundle service: %v", err)
	}

	h.provider, err = sysbox.New(runner, bundles, h.stateDir)
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
// No network: sysbox-runc makes a namespace of its own, which is all the suite needs.
func (h *harness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	id := fmt.Sprintf("shard-93-%d", h.next.Add(1))
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

	spec := models.SandboxSpec{
		ID:         id,
		StateDir:   dir,
		RootFS:     h.image.RootFS,
		Entrypoint: entrypoint,
	}

	return runspec.Resolve(spec, h.image.Config)
}

func requireSysboxRunc(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("sysbox-runc needs root")
	}
	if _, err := exec.LookPath("sysbox-runc"); err != nil {
		t.Skip("no sysbox-runc on this host")
	}
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
	if err := hostclean.Refuse(stateRoots()...); err != nil {
		fmt.Fprintln(os.Stderr, "sysbox integration tests:", err)
		os.Exit(1)
	}
	teardownOnSignal()

	root, err := os.MkdirTemp("", "shard-93-images")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create the image root:", err)
		os.Exit(1)
	}
	imageRoot = root

	code := m.Run()

	if err := teardown(); err != nil {
		fmt.Fprintln(os.Stderr, "give the host back:", err)

		code = 1
	}

	os.Exit(code)
}

// stateRoots are the roots this package makes. One left behind means an earlier run kept host state.
func stateRoots() []string { return []string{filepath.Join(os.TempDir(), "shard-93-images")} }

// tempPrefixes adds the scratch directory of an exec, which a killed run leaves and nothing pins.
func tempPrefixes() []string { return append(stateRoots(), filepath.Join(os.TempDir(), "shard-exec-")) }

var torndown sync.Once

// teardown gives the host back: the cgroups a failed create left, then the mounts and the roots.
func teardown() error {
	var err error
	torndown.Do(func() {
		sweepCgroups()
		err = hostclean.Sweep(tempPrefixes()...)
	})

	return err
}

// teardownOnSignal gives the host back when the run is interrupted. 130 is what a shell reports
// for a run that a SIGINT ended.
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

// sweepCgroups removes what a failed create leaves at the cgroup root. A cgroup left behind carries
// its counters into the next sandbox that takes the same id, and the ids here repeat on every run.
func sweepCgroups() {
	left, err := filepath.Glob(filepath.Join(cgroup.Root, bundle.CgroupsPath("shard-93-*")))
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
