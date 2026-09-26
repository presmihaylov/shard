package daemon

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/vzshim"
	"github.com/presmihaylov/shard/services/network"
)

// The daemon supervises its tasks at once over one deps, so two of them can ask for the same layer at
// the same moment. Run this one under -race: without the lock the detector names the memoised fields.
func TestTheGettersBuildOneLayerUnderConcurrentAsks(t *testing.T) {
	d := &deps{cfg: Config{Root: t.TempDir()}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			repo, err := d.repo()
			if err != nil {
				t.Errorf("repo: %v", err)
			}

			secrets, err := d.secrets()
			if err != nil {
				t.Errorf("secrets: %v", err)
			}

			if _, err := d.egress(); err != nil {
				t.Errorf("egress: %v", err)
			}

			// One daemon holds one of each, or the proxy judges against a store the API never writes to.
			if repo != d.repoSvc || secrets != d.secretSvc {
				t.Error("a getter built a second copy of a layer the daemon already held")
			}
		})
	}
	wg.Wait()
}

// --provider picks the substrate by name. A name shard does not know fails the first ask, not a sandbox.
func TestTheProviderIsPickedByName(t *testing.T) {
	// Both runners look their binary up on PATH, and neither substrate is installed where the tests run.
	bin := t.TempDir()
	for _, binary := range []string{"runsc", "sysbox-runc", "runc"} {
		if err := os.WriteFile(filepath.Join(bin, binary), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatalf("write the fake %s: %v", binary, err)
		}
	}
	t.Setenv("PATH", bin)

	for _, name := range []string{"gvisor", "sysbox", "runc"} {
		d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: name}}

		provider, err := d.providerLocked()
		if err != nil {
			t.Fatalf("provider %q: %v", name, err)
		}
		if got := provider.Name(); got != name {
			t.Errorf("--provider %q built %s, want %s", name, got, name)
		}
	}

	d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "vmware"}}
	if _, err := d.providerLocked(); err == nil || !strings.Contains(err.Error(), `unknown provider "vmware"`) {
		t.Errorf("an unknown provider built %v, want a refusal that names it", err)
	}
}

// firecracker is refused by name off Linux, and on Linux without its binary, before any kernel is fetched.
func TestFirecrackerIsRefusedWithoutItsPlatformOrItsBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "firecracker"}}

	_, err := d.providerLocked()
	want := "needs firecracker on PATH"
	if runtime.GOOS != "linux" {
		want = "Linux only"
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("firecracker on %s built %v, want a refusal that says %q", runtime.GOOS, err, want)
	}
}

// A fresh data root has no firecracker directory yet, and the provider makes its own before it writes the initrd there.
func TestFirecrackerBuildsOnAFreshRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("firecracker is refused on %s before any directory is made", runtime.GOOS)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "firecracker"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a stand-in on PATH must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	body := []byte("dev kernel")
	if err := os.WriteFile(kernel, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHARD_KERNEL", kernel)
	t.Setenv("SHARD_KERNEL_SHA256", fmt.Sprintf("%x", sha256.Sum256(body)))
	init := filepath.Join(t.TempDir(), "shard-init")
	if err := os.WriteFile(init, []byte("init"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "root")
	d := &deps{cfg: Config{Root: root, InitPath: init, Provider: "firecracker"}}
	if _, err := d.providerLocked(); err != nil {
		t.Fatalf("firecracker on a fresh root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, firecrackerDir, "initrd.cpio")); err != nil {
		t.Fatalf("the initrd after the build: %v", err)
	}
}

// SHARD-239: vz named off a Mac is refused, never downgraded. What an empty --provider picks is in select_test.go.
func TestVZIsRefusedOffAMac(t *testing.T) {
	v := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "vz"}}
	_, err := v.providerLocked()
	if runtime.GOOS != "darwin" {
		if err == nil || !strings.Contains(err.Error(), "macOS only") {
			t.Fatalf("vz on %s built %v, want a refusal naming macOS", runtime.GOOS, err)
		}

		return
	}
	// A go build alone carries no shim, and the refusal says which make target does; a make build-darwin binary goes on to the kernel.
	if !vzshim.Embedded() && (err == nil || !strings.Contains(err.Error(), "make build-darwin")) {
		t.Fatalf("vz without the shim built %v, want a refusal naming make build-darwin", err)
	}
}

// A vz daemon leases addresses from a pool with no bridge, and its proxy listens on the stack, not the host.
func TestAVZDaemonLeasesAddressesAndFrontsOnTheStack(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vz is a darwin provider")
	}
	d := &deps{cfg: Config{Root: t.TempDir(), Provider: "vz"}}

	hostNet, err := d.net()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hostNet.(*network.Addresses); !ok {
		t.Fatalf("a vz daemon built %T for its network, want the address pool", hostNet)
	}
	spec, err := hostNet.Allocate(t.Context(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Gateway != hostNet.Gateway() || spec.NetnsPath != "" {
		t.Errorf("leased %+v, want the gateway %s and no namespace", spec, hostNet.Gateway())
	}

	f, err := d.front()
	if err != nil {
		t.Fatal(err)
	}
	if f != any(d.stackSvc) {
		t.Fatalf("the front is %T, want the daemon's stack", f)
	}
	ln, err := f.ListenTCP(30080)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if d.stackSvc.Address() != hostNet.Gateway() {
		t.Errorf("the stack answers for %s, the pool hands out gateway %s", d.stackSvc.Address(), hostNet.Gateway())
	}
	// The stack judges by the same chains the leases compile, so a flow off the gateway gets the Linux answer: the floor drops, the rest passes without a policy.
	if d.addressesSvc == nil {
		t.Fatal("the vz daemon holds no address pool of its own")
	}
	guest := spec.Address.Addr()
	if v := d.addressesSvc.Judge(netstack.Flow{Guest: guest, Protocol: "tcp", Destination: netip.MustParseAddrPort("192.168.1.1:22")}); v.Allow || v.Rule != network.RulePrivate {
		t.Errorf("a private destination judged %+v", v)
	}
	if v := d.addressesSvc.Judge(netstack.Flow{Guest: guest, Protocol: "tcp", Destination: netip.MustParseAddrPort("203.0.113.7:22")}); !v.Allow || v.Rule != network.RuleNone {
		t.Errorf("a public destination without a policy judged %+v", v)
	}
}

// SHARD-93: a Sysbox host has no runsc, and the daemon must not need one. What the runtime keeps under
// its root, and whether the guest owns its namespaces, are the provider's to say, not the daemon's.
func TestASysboxDaemonNeedsNoRunsc(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sysbox-runc"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the fake sysbox-runc: %v", err)
	}
	t.Setenv("PATH", bin)

	d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "sysbox"}}

	if _, err := d.substrateLocked(); err != nil {
		t.Fatalf("the substrate hook of a sysbox daemon: %v", err)
	}

	got, err := d.userns()
	if err != nil {
		t.Fatalf("the userns of a sysbox daemon: %v", err)
	}
	if got.HostID != 165536 || got.Size != 65536 {
		t.Errorf("sysbox userns is %+v, want the Sysbox CE mapping 165536+65536", got)
	}

	g := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "gvisor"}}
	if _, err := g.substrateLocked(); err == nil || !strings.Contains(err.Error(), "runsc") {
		t.Errorf("a gvisor daemon without runsc built its substrate with %v, want a refusal naming runsc", err)
	}
	if _, err := g.userns(); err == nil || !strings.Contains(err.Error(), "runsc") {
		t.Errorf("a gvisor daemon without runsc answered its userns with %v, want a refusal naming runsc", err)
	}
}

// The userns is the provider's answer, so the network cannot be built before the substrate is
// found. It is asked at Allocate instead, and a gVisor daemon answers it with no mapping at all.
func TestTheUsernsIsAskedOfTheProviderNotTheName(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "runsc"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the fake runsc: %v", err)
	}
	t.Setenv("PATH", bin)

	d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "gvisor"}}

	got, err := d.userns()
	if err != nil {
		t.Fatalf("the userns of a gvisor daemon: %v", err)
	}
	if got.Set() {
		t.Errorf("a gvisor daemon owns its namespaces from %+v, want the host's user namespace", got)
	}
}

// A deps with no Out is what a test builds, and the kernel fetch logs through it, so it must not panic.
func TestADepsWithNoOutLogsNowhere(t *testing.T) {
	d := &deps{cfg: Config{Root: t.TempDir()}}
	d.logger().Print("dropped")

	var out strings.Builder
	d = &deps{cfg: Config{Root: t.TempDir(), Out: &out}}
	d.logger().Print("kept")
	if !strings.Contains(out.String(), "kept") {
		t.Fatalf("log = %q, want the line", out.String())
	}
}
