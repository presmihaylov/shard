//go:build integration && darwin && arm64

package vzvm_test

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/pkg/vzshim"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
)

// The suite wants the shard kernel; a Mac without one skips it, and SHARD_KERNEL names one elsewhere.
const defaultKernel = "../../../bin/kernel/arm64/Image-arm64"

const testImage = "alpine:3.20"

var gateway = netip.MustParseAddr("10.200.0.1")

// vmHarness is the provider over real VMs: the shim, the kernel, a linux/arm64 shard-init and the image's disk.
type vmHarness struct {
	provider *vzvm.Provider
	root     string
	image    image.Image
	next     atomic.Int64
}

func newVMHarness(t *testing.T) *vmHarness {
	t.Helper()

	kernel := os.Getenv("SHARD_KERNEL")
	if kernel == "" {
		kernel = defaultKernel
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("no guest kernel at %s: build one with make kernel, or set SHARD_KERNEL", kernel)
	}

	root, err := os.MkdirTemp("", "vz") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	svc, err := image.New(filepath.Join(root, "images"), image.WithDisks())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	img, err := svc.Pull(ctx, testImage)
	if err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}

	stack, err := netstack.New(netstack.Config{Address: gateway})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stack.Close() })

	h := &vmHarness{root: root, image: img}
	h.provider, err = vzvm.New(vzvm.Config{
		Shim:        shimBinary(t),
		Kernel:      kernel,
		Init:        guestInit(t),
		Dir:         root,
		Stack:       stack,
		Dirs:        h.stateDir,
		SaveRestore: vz.HostSaveRestore() && !sessionLocked(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func (h *vmHarness) stateDir(id string) (string, error) {
	return filepath.Join(h.root, "s", id), nil
}

// newSpec gives every sandbox its own id, directory and address on the stack, and ends it when the test does.
func (h *vmHarness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	n := h.next.Add(1)
	id := fmt.Sprintf("vm-%d", n)
	dir, _ := h.stateDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Best effort: a subtest may have stopped and removed this one already, and its errors say nothing new.
		ctx := context.Background()
		h.provider.Stop(ctx, id, stopGrace)
		h.provider.Remove(ctx, id)
	})

	return models.SandboxSpec{
		ID:         id,
		StateDir:   dir,
		RootFS:     h.image.RootFS,
		RootDisk:   h.image.Disk,
		Entrypoint: entrypoint,
		Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Network:    models.NetworkSpec{Address: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 200, 0, byte(n + 1)}), 24), Gateway: gateway},
		Resources:  models.Resources{MemoryMiB: 256, DiskMiB: 64},
	}
}

func TestConformanceOnVMs(t *testing.T) {
	h := newVMHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/true") },
		NewIgnoresTermSpec: func(t *testing.T) models.SandboxSpec {
			script := fmt.Sprintf("trap '' TERM; echo %s; while true; do sleep 1; done", conformance.ReadyMarker)

			return h.newSpec(t, "/bin/sh", "-c", script)
		},
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
	})
}

// A resumed VM carries its memory: the counter the entrypoint kept goes on from where the pause froze it.
func TestAResumeAndAForkCarryTheGuestMemory(t *testing.T) {
	h := newVMHarness(t)
	if !h.provider.Capabilities().Fork {
		t.Skip("this Mac does not save a VM")
	}
	spec := h.newSpec(t, "/bin/sh", "-c", "i=0; while true; do i=$((i+1)); echo $i > /count; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	fork := h.newSpec(t)
	if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{spec.ID, fork.ID} {
		out, err := os.CreateTemp(t.TempDir(), "count")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		if _, err := h.provider.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "cat /count; ip -4 -o addr show eth0"}, Stdout: out, Stderr: out}); err != nil {
			t.Fatalf("Exec in %s: %v", id, err)
		}
		written, _ := os.ReadFile(out.Name())
		fields := strings.Fields(string(written))
		if len(fields) < 1 || fields[0] == "" || fields[0] == "0" {
			t.Fatalf("%s counted %q after the restore, want a count the pause froze", id, written)
		}
		t.Logf("%s: %s", id, strings.TrimSpace(string(written)))
	}
}

// The shim needs the virtualization entitlement, and the embedded one is signed on install; a build without it is signed here.
func shimBinary(t *testing.T) string {
	t.Helper()

	if shim := os.Getenv("SHARD_VZ_SHIM"); shim != "" {
		return shim
	}
	if vzshim.Embedded() {
		shim, err := vzshim.Install(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		return shim
	}
	shim := filepath.Join(t.TempDir(), "shard-vz-shim")
	run(t, "go", "build", "-o", shim, "../../../cmd/shard-vz-shim")
	run(t, "codesign", "--sign", "-", "--force", "--entitlements", "../../../pkg/vzshim/shim/entitlements.plist", shim)

	return shim
}

// guestInit is shard-init for the VM: static linux/arm64, which the provider packs as the initrd's /init.
func guestInit(t *testing.T) string {
	t.Helper()

	if init := os.Getenv("SHARD_VZ_INIT"); init != "" {
		return init
	}
	path := filepath.Join(t.TempDir(), "shard-init")
	build := exec.Command("go", "build", "-o", path, "../../../cmd/shard-init")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shard-init: %v: %s", err, out)
	}

	return path
}

// On macOS 14 a locked screen withholds the key a restore needs (docs/provider-vz.md, item 9); 26.6 restores locked, so only 14 falls back to the freeze path.
func sessionLocked(t *testing.T) bool {
	t.Helper()

	version, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		t.Fatalf("sw_vers: %v", err)
	}
	if !strings.HasPrefix(string(version), "14.") {
		return false
	}
	out, err := exec.Command("ioreg", "-n", "Root", "-d1", "-a").Output()
	if err != nil {
		t.Fatalf("ioreg: %v", err)
	}

	return regexp.MustCompile(`CGSSessionScreenIsLocked</key>\s*<true/>`).Match(out)
}

func run(t *testing.T, argv ...string) {
	t.Helper()

	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}
}
