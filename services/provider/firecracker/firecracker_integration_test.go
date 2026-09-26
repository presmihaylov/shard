//go:build integration && linux

package firecracker_test

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/erofs"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/firecracker"
)

// The suite wants the shard kernel; a box without one skips it, and SHARD_KERNEL names one elsewhere.
const defaultKernel = "../../../bin/kernel/amd64/vmlinux-amd64"

// The digest is alpine:3.20 as of 2026-09-20; a tag moves.
const testImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// vmHarness is the provider over real microVMs: the kernel, a static shard-init and the image's EROFS file.
type vmHarness struct {
	*harness
	kernel string
	image  image.Image
}

// requireKVM skips unless this process can boot a microVM: root, /dev/kvm, and the two binaries the boot needs.
func requireKVM(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("firecracker integration tests need root")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("no /dev/kvm: %v", err)
	}
	for _, binary := range []string{firecracker.Binary, erofs.Tool} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is not on PATH: %v", binary, err)
		}
	}
}

func newVMHarness(t *testing.T) *vmHarness {
	t.Helper()
	requireKVM(t)

	kernel := os.Getenv("SHARD_KERNEL")
	if kernel == "" {
		kernel = defaultKernel
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("no guest kernel at %s: build one with make kernel ARCH=amd64, or set SHARD_KERNEL", kernel)
	}

	root, err := os.MkdirTemp("", "fc") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	svc, err := image.New(filepath.Join(root, "images"), image.WithErofs())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	img, err := svc.Pull(ctx, testImage)
	if err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}

	h := &vmHarness{harness: &harness{root: root, erofs: img.Erofs}, kernel: kernel, image: img}
	h.open(t)

	return h
}

// open is a daemon start over the real vmm, in place of the fake the unit harness execs.
func (h *vmHarness) open(t *testing.T) *firecracker.Provider {
	t.Helper()

	p, err := firecracker.New(firecracker.Config{
		Binary: firecracker.Binary,
		Kernel: h.kernel,
		Init:   guestInit(t),
		Dir:    h.root,
		Dirs:   h.stateDir,
	})
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}
	h.provider = p

	return p
}

func (h *vmHarness) reopen(t *testing.T) models.Provider {
	t.Helper()

	if err := h.provider.Close(); err != nil {
		t.Fatalf("close the provider: %v", err)
	}

	return h.open(t)
}

// newSpec is the unit harness's over the pulled image: the tree names users for exec, and the entrypoint needs a PATH.
func (h *vmHarness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	spec := h.harness.newSpec(t, entrypoint...)
	spec.RootFS = h.image.RootFS
	spec.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	spec.Resources = models.Resources{MemoryMiB: 256, DiskMiB: 64}

	return spec
}

// guestInit is a static shard-init for the box's arch, which the initrd runs as PID 1; SHARD_FC_INIT names a built one.
func guestInit(t *testing.T) string {
	t.Helper()

	if init := os.Getenv("SHARD_FC_INIT"); init != "" {
		return init
	}
	path := filepath.Join(t.TempDir(), "shard-init")
	build := exec.Command("go", "build", "-o", path, "../../../cmd/shard-init")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shard-init: %v: %s", err, out)
	}

	return path
}

// The boot AC: the guest kernel mounts the EROFS image under the overlay, and the entrypoint runs and writes to the log.
func TestAMicroVMBootsAndRunsTheEntrypoint(t *testing.T) {
	h := newVMHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "echo booted on $(uname -r); touch /written && cat /etc/alpine-release")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
	if err != nil || exit.Code != 0 {
		console, _ := os.ReadFile(filepath.Join(spec.StateDir, "console.log"))
		t.Fatalf("Wait = %+v, %v\nsandbox log:\n%s\nconsole:\n%s", exit, err, log, console)
	}
	if !strings.Contains(string(log), "booted on") || !strings.Contains(string(log), "3.20") {
		t.Fatalf("the entrypoint did not run over the image:\n%s", log)
	}
}

// A tap on the test bridge, leased the way the daemon leases one for a microVM; the bridge and the tables go with the test.
func newTapNetwork(t *testing.T) *network.Service {
	t.Helper()

	for _, binary := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("no %s on this host", binary)
		}
	}
	manager, err := netns.New()
	if err != nil {
		t.Fatalf("open the netns manager: %v", err)
	}
	svc, err := network.New(network.Config{
		Root: t.TempDir(), Bridge: testBridge, Subnet: netip.MustParsePrefix(testSubnet), Tap: true,
	}, manager)
	if err != nil {
		t.Fatalf("open the network service: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := manager.DeleteLink(ctx, testBridge); err != nil {
			t.Logf("remove the test bridge: %v", err)
		}
		for _, family := range []string{"inet", "bridge"} {
			if err := manager.DeleteTable(ctx, family, "shard"); err != nil {
				t.Logf("remove the test %s table: %v", family, err)
			}
		}
	})

	return svc
}

const (
	testBridge = "shardt0"
	testSubnet = "10.213.0.0/24"
)

// The network AC: the guest takes the leased address over the tap, and its frames reach the bridge with the MAC the lease fixes.
func TestAMicroVMIsAddressedOverItsTap(t *testing.T) {
	h := newVMHarness(t)
	tapNet := newTapNetwork(t)

	// The host's input chain drops the ping, but the ARP under it lands the guest's MAC on the bridge.
	spec := h.newSpec(t, "/bin/sh", "-c",
		"ip -4 -o addr show eth0; ip route show default; ping -c 1 -W 1 10.213.0.1; ip neigh show; hostname")
	spec.Name = "web"
	lease, err := tapNet.Allocate(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tapNet.Release(context.Background(), spec.ID); err != nil {
			t.Logf("release the lease: %v", err)
		}
	})
	if lease.Address.String() != "10.213.0.2/24" || lease.HostInterface != "shardv2" {
		t.Fatalf("the lease is %+v, want the first address of %s over shardv2", lease, testSubnet)
	}
	spec.Network = lease

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	// The log appends and the bridge keeps a neighbour, so each boot must add its own proof.
	for boots, phase := range []string{"the first boot", "a boot after a stop"} {
		// The daemon leases the network again before every start, which builds the tap again for the new vmm.
		if boots > 0 {
			if _, err := tapNet.Allocate(t.Context(), spec.ID); err != nil {
				t.Fatalf("%s: allocate again: %v", phase, err)
			}
		}
		if out, err := exec.Command("ip", "neigh", "flush", "dev", testBridge).CombinedOutput(); err != nil {
			t.Fatalf("%s: flush the bridge neighbours: %v: %s", phase, err, out)
		}
		if err := h.provider.Start(t.Context(), spec.ID); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		exit, err := h.provider.Wait(t.Context(), spec.ID)
		log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
		if err != nil || exit.Code != 0 {
			console, _ := os.ReadFile(filepath.Join(spec.StateDir, "console.log"))
			t.Fatalf("%s: Wait = %+v, %v\nsandbox log:\n%s\nconsole:\n%s", phase, exit, err, log, console)
		}
		for _, want := range []string{"inet 10.213.0.2/24", "default via 10.213.0.1", "10.213.0.1 dev eth0 lladdr", "\nweb\n"} {
			if got := strings.Count(string(log), want); got != boots+1 {
				t.Errorf("%s: the guest showed %q %d times, want %d:\n%s", phase, want, got, boots+1, log)
			}
		}
		neigh, err := exec.Command("ip", "neigh", "show", "dev", testBridge).CombinedOutput()
		if err != nil || !strings.Contains(string(neigh), "10.213.0.2 lladdr 02:fc:0a:d5:00:02") {
			t.Errorf("%s: the bridge did not learn the guest from its tap: %v\n%s", phase, err, neigh)
		}

		if err := h.provider.Stop(t.Context(), spec.ID, time.Second); err != nil {
			t.Fatalf("%s: stop: %v", phase, err)
		}
	}
}

func TestConformanceOnMicroVMs(t *testing.T) {
	h := newVMHarness(t)
	requireReflink(t, h.root)

	conformance.Run(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/true") },
		NewIgnoresTermSpec: func(t *testing.T) models.SandboxSpec {
			script := fmt.Sprintf("trap '' TERM; echo %s; while true; do sleep 1; done", conformance.ReadyMarker)

			return h.newSpec(t, "/bin/sh", "-c", script)
		},
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
		Reopen:      h.reopen,
	})
}
