//go:build integration && linux

package firecracker_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// tickScript counts in the shell's own memory, so a guest that restored carries on and one that booted again starts over at one.
const tickScript = "n=0; while true; do n=$((n+1)); echo tick $n; sleep 0.1; done"

// ticks is every count the entrypoint has printed into the log so far.
func ticks(t *testing.T, log string) []int {
	t.Helper()

	blob, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var counted []int
	for line := range strings.SplitSeq(string(blob), "\n") {
		count, found := strings.CutPrefix(strings.TrimSpace(line), "tick ")
		if !found {
			continue
		}
		// A restore re-sends what a write was cut in the middle of, which can leave a part of a line behind.
		n, err := strconv.Atoi(count)
		if err != nil {
			continue
		}
		counted = append(counted, n)
	}

	return counted
}

// awaitTicks waits for the log to hold want counts.
func awaitTicks(t *testing.T, log string, want int) []int {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		counted := ticks(t, log)
		if len(counted) >= want {
			return counted
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %v after 30s, want %d counts", log, counted, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fresh is how many times the entrypoint started counting; a guest that restored its memory never starts again.
func fresh(counted []int) int {
	starts := 0
	for _, count := range counted {
		if count == 1 {
			starts++
		}
	}

	return starts
}

// runIn runs one command in a sandbox and gives back how it ended and everything it wrote.
func runIn(t *testing.T, p models.Provider, id, script string) (models.ExitStatus, string) {
	t.Helper()

	out, err := os.CreateTemp(t.TempDir(), "exec-output")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close the exec output file: %v", err)
		}
	}()

	status, err := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", script}, Stdout: out, Stderr: out})
	if err != nil {
		t.Fatalf("exec %q in %s: %v", script, id, err)
	}
	written, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}

	return status, string(written)
}

// allocate leases an address the way the daemon does before every start, and gives it back with the test.
func allocate(t *testing.T, svc *network.Service, id string) models.NetworkSpec {
	t.Helper()

	leased, err := svc.Allocate(t.Context(), id)
	if err != nil {
		t.Fatalf("allocate for %s: %v", id, err)
	}
	t.Cleanup(func() {
		if err := svc.Release(context.Background(), id); err != nil {
			t.Logf("release the lease of %s: %v", id, err)
		}
	})

	return leased
}

// The pause AC: the vCPUs stop and the memory lands on disk, and the resume brings that memory back with the guest counting on from it.
func TestAMicroVMResumesFromItsSnapshotWithItsMemory(t *testing.T) {
	h := newVMHarness(t)
	requireReflink(t, h.root)

	spec := h.newSpec(t, "/bin/sh", "-c", tickScript)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(spec.StateDir, "output.log")
	awaitTicks(t, log, 3)

	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		console, _ := os.ReadFile(filepath.Join(spec.StateDir, "console.log"))
		t.Fatalf("Pause: %v\nconsole:\n%s", err, console)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after the Pause = %+v, %v, want %s", status, err, models.StateStopped)
	}
	paused := ticks(t, log)
	time.Sleep(time.Second)
	if now := ticks(t, log); len(now) != len(paused) {
		t.Fatalf("the guest counted %d more while paused, want none", len(now)-len(paused))
	}

	began := time.Now()
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Logf("the resume took %s", time.Since(began))
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the Resume = %+v, %v, want %s", status, err, models.StateRunning)
	}
	counted := awaitTicks(t, log, len(paused)+2)
	if fresh(counted) != 1 {
		t.Errorf("the entrypoint started counting %d times, want once: %v", fresh(counted), counted)
	}
	if last := counted[len(counted)-1]; last <= paused[len(paused)-1] {
		t.Errorf("the guest is at %d after the resume, want it past the %d it paused at", last, paused[len(paused)-1])
	}
}

// The fork AC: one snapshot brings up many sandboxes, each with the source's memory and its own copy of the disk, and the source and the snapshot outlive them all.
func TestManyMicroVMsForkFromOneSnapshot(t *testing.T) {
	h := newVMHarness(t)
	requireReflink(t, h.root)

	spec := h.newSpec(t, "/bin/sh", "-c", tickScript)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(spec.StateDir, "output.log")
	awaitTicks(t, log, 3)

	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	paused := ticks(t, log)

	forks := []models.SandboxSpec{h.forkSpec(t), h.forkSpec(t), h.forkSpec(t)}
	for _, fork := range forks {
		began := time.Now()
		if err := h.provider.Fork(t.Context(), dir, fork); err != nil {
			t.Fatalf("Fork into %s: %v", fork.ID, err)
		}
		// hypeman takes about 62ms for the same verb over the same substrate.
		t.Logf("the fork into %s took %s", fork.ID, time.Since(began))
	}

	for _, fork := range forks {
		status, err := h.provider.Status(t.Context(), fork.ID)
		if err != nil || status.State != models.StateRunning {
			t.Fatalf("Status of %s = %+v, %v, want %s", fork.ID, status, err, models.StateRunning)
		}
		// Nothing started this entrypoint: it is the source's, carrying on out of the memory the fork restored.
		counted := awaitTicks(t, filepath.Join(fork.StateDir, "output.log"), 2)
		if fresh(counted) != 0 {
			t.Errorf("%s started counting from one, so it booted instead of restoring: %v", fork.ID, counted)
		}
		if status, out := runIn(t, h.provider, fork.ID, "echo "+fork.ID+" > /forked; cat /forked"); status.Code != 0 {
			t.Fatalf("the write into %s = %+v: %s", fork.ID, status, out)
		}
	}
	// Each fork took its own copy of the overlay, so none of them sees what another wrote.
	for _, fork := range forks {
		if _, out := runIn(t, h.provider, fork.ID, "cat /forked"); strings.TrimSpace(out) != fork.ID {
			t.Errorf("%s reads /forked as %q, want its own id", fork.ID, out)
		}
	}

	// The snapshot is not consumed: the source comes back from the same one, and holds nothing a fork wrote.
	if err := h.provider.Resume(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Resume after the forks: %v", err)
	}
	if status, out := runIn(t, h.provider, spec.ID, "cat /forked"); status.Code == 0 {
		t.Errorf("the source reads /forked as %q, and the file is a fork's alone", out)
	}
	if counted := awaitTicks(t, log, len(paused)+1); fresh(counted) != 1 {
		t.Errorf("the source started counting again after the forks: %v", counted)
	}
}

// The fork AC for the network: the restored guest holds the source's address, and the fork replaces it with the lease's own, MAC and name and all.
func TestAForkTakesItsOwnAddress(t *testing.T) {
	h := newVMHarness(t)
	requireReflink(t, h.root)
	tapNet := newTapNetwork(t)

	spec := h.newSpec(t, "/bin/sh", "-c", tickScript)
	spec.Name = "source"
	spec.Network = allocate(t, tapNet, spec.ID)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	awaitTicks(t, filepath.Join(spec.StateDir, "output.log"), 2)

	dir := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, dir); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	fork := h.forkSpec(t)
	fork.Name = "forked"
	fork.Network = allocate(t, tapNet, fork.ID)
	if fork.Network.Address.String() != "10.213.0.3/24" || fork.Network.HostInterface != "shardv3" {
		t.Fatalf("the fork's lease is %+v, want the second address of %s over shardv3", fork.Network, testSubnet)
	}
	if err := h.provider.Fork(t.Context(), dir, fork); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if out, err := exec.Command("ip", "neigh", "flush", "dev", testBridge).CombinedOutput(); err != nil {
		t.Fatalf("flush the bridge neighbours: %v: %s", err, out)
	}

	// The host's input chain drops the ping, but the ARP under it lands the fork's MAC on the bridge.
	_, out := runIn(t, h.provider, fork.ID,
		"ip -4 -o addr show eth0; ip link show eth0; ip route show default; ping -c 1 -W 1 10.213.0.1; hostname")
	for _, want := range []string{"inet 10.213.0.3/24", "02:fc:0a:d5:00:03", "default via 10.213.0.1", "\nforked\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the fork does not show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "10.213.0.2/24") || strings.Contains(out, "02:fc:0a:d5:00:02") {
		t.Errorf("the fork still holds what the source restored with:\n%s", out)
	}
	neigh, err := exec.Command("ip", "neigh", "show", "dev", testBridge).CombinedOutput()
	if err != nil || !strings.Contains(string(neigh), "10.213.0.3 lladdr 02:fc:0a:d5:00:03") {
		t.Errorf("the bridge did not learn the fork from its tap: %v\n%s", err, neigh)
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
