//go:build integration

package network_test

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/network"
)

// The test bridge and subnet are its own, so a run here never disturbs the sandboxes on shard0.
const (
	testBridge = "shardt0"
	testSubnet = "10.213.0.0/24"
)

func TestEnsureBuildsTheBridgeTheForwardingAndTheTable(t *testing.T) {
	s, m := newService(t)

	if err := s.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	exists, err := m.LinkExists(t.Context(), testBridge)
	if err != nil {
		t.Fatalf("LinkExists: %v", err)
	}
	if !exists {
		t.Errorf("Ensure did not create the bridge %s", testBridge)
	}

	if got := readFile(t, "/proc/sys/net/ipv4/ip_forward"); strings.TrimSpace(got) != "1" {
		t.Errorf("ip_forward is %q, and nothing leaves the bridge without it", strings.TrimSpace(got))
	}

	table, err := m.TableExists(t.Context(), "inet", "shard")
	if err != nil {
		t.Fatalf("TableExists: %v", err)
	}
	if !table {
		t.Error("Ensure did not apply the inet shard table")
	}
}

// Every Allocate calls Ensure, so a second one must not fail on what the first built.
func TestEnsureIsIdempotent(t *testing.T) {
	s, _ := newService(t)

	for _, when := range []string{"first", "second"} {
		if err := s.Ensure(t.Context()); err != nil {
			t.Fatalf("the %s Ensure: %v", when, err)
		}
	}
}

func TestAllocateBuildsTheNamespaceAndItsRoute(t *testing.T) {
	s, m := newService(t)
	spec := allocate(t, s, "amber-otter")

	if _, err := os.Stat(spec.NetnsPath); err != nil {
		t.Fatalf("the namespace at %s: %v", spec.NetnsPath, err)
	}

	addresses, err := m.AddressesIn(t.Context(), "amber-otter", "eth0")
	if err != nil {
		t.Fatalf("AddressesIn: %v", err)
	}
	if !slices.Contains(addresses, spec.Address) {
		t.Errorf("eth0 carries %v, want %s", addresses, spec.Address)
	}

	route := run(t, "ip", "-netns", "amber-otter", "route", "show", "default")
	if !strings.Contains(route, spec.Gateway.String()) {
		t.Errorf("the default route is %q, want it via %s", strings.TrimSpace(route), spec.Gateway)
	}

	// -details is the only way to read the flag back, and it is what keeps two sandboxes apart.
	link := run(t, "ip", "-details", "link", "show", spec.HostInterface)
	if !strings.Contains(link, "isolated on") {
		t.Errorf("%s is not an isolated bridge port: %q", spec.HostInterface, strings.TrimSpace(link))
	}
	if !strings.Contains(link, "master "+testBridge) {
		t.Errorf("%s is not a port of %s: %q", spec.HostInterface, testBridge, strings.TrimSpace(link))
	}
}

func TestTwoSandboxesGetTheirOwnAddressAndInterface(t *testing.T) {
	s, _ := newService(t)

	first := allocate(t, s, "amber-otter")
	second := allocate(t, s, "brisk-heron")

	if first.Address == second.Address {
		t.Errorf("both sandboxes got %s", first.Address)
	}
	if first.HostInterface == second.HostInterface {
		t.Errorf("both sandboxes got the host interface %s", first.HostInterface)
	}
	if first.NetnsPath == second.NetnsPath {
		t.Errorf("both sandboxes got the namespace %s", first.NetnsPath)
	}
}

// A sandbox that stops and starts again must come back on the address its record already names.
func TestASecondAllocateReturnsTheSameNetwork(t *testing.T) {
	s, _ := newService(t)

	first := allocate(t, s, "amber-otter")
	second := allocate(t, s, "amber-otter")

	if !reflect.DeepEqual(first, second) {
		t.Errorf("got %+v then %+v, want the same network twice", first, second)
	}
}

// A host reboot keeps the lease under /var/lib/shard and drops /var/run/netns, so Allocate must
// rebuild the namespace at the address the lease still names.
func TestAllocateRebuildsANamespaceThatIsGone(t *testing.T) {
	s, m := newService(t)
	first := allocate(t, s, "amber-otter")

	if err := m.DeleteNamespace(t.Context(), "amber-otter"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}

	second := allocate(t, s, "amber-otter")
	if second.Address != first.Address {
		t.Errorf("the rebuild moved the sandbox from %s to %s", first.Address, second.Address)
	}
	if _, err := os.Stat(second.NetnsPath); err != nil {
		t.Fatalf("Allocate did not rebuild the namespace: %v", err)
	}
}

func TestReleaseDropsTheNamespaceTheLinkAndTheLease(t *testing.T) {
	s, m := newService(t)
	spec := allocate(t, s, "amber-otter")

	if err := s.Release(t.Context(), "amber-otter"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := os.Stat(spec.NetnsPath); !os.IsNotExist(err) {
		t.Errorf("the namespace at %s survived the release: %v", spec.NetnsPath, err)
	}

	exists, err := m.LinkExists(t.Context(), spec.HostInterface)
	if err != nil {
		t.Fatalf("LinkExists: %v", err)
	}
	if exists {
		t.Errorf("the host interface %s survived the release", spec.HostInterface)
	}

	// The lease is what the next sandbox needs back, and only the address it gets can prove it is free.
	next := allocate(t, s, "brisk-heron")
	if next.Address != spec.Address {
		t.Errorf("the next sandbox got %s, want the released %s", next.Address, spec.Address)
	}
}

// Release is what a stop calls, so it must answer the same way on a sandbox that never allocated one.
func TestReleaseIsIdempotent(t *testing.T) {
	s, _ := newService(t)

	if err := s.Release(t.Context(), "never-allocated"); err != nil {
		t.Fatalf("Release without a lease: %v", err)
	}

	allocate(t, s, "amber-otter")
	for _, when := range []string{"first", "second"} {
		if err := s.Release(t.Context(), "amber-otter"); err != nil {
			t.Fatalf("the %s Release: %v", when, err)
		}
	}
}

func newService(t *testing.T) (*network.Service, *netns.Manager) {
	t.Helper()
	requireNetworkTools(t)

	m, err := netns.New()
	if err != nil {
		t.Fatalf("open the netns manager: %v", err)
	}

	s, err := network.New(network.Config{
		Root:   t.TempDir(),
		Bridge: testBridge,
		Subnet: netip.MustParsePrefix(testSubnet),
	}, m)
	if err != nil {
		t.Fatalf("open the network service: %v", err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		// Best effort: the bridge and the table are the host state a failed test would otherwise leave.
		if err := m.DeleteLink(ctx, testBridge); err != nil {
			t.Logf("remove the test bridge: %v", err)
		}
		if err := m.DeleteTable(ctx, "inet", "shard"); err != nil {
			t.Logf("remove the test table: %v", err)
		}
	})

	return s, m
}

func allocate(t *testing.T, s *network.Service, id string) models.NetworkSpec {
	t.Helper()

	spec, err := s.Allocate(t.Context(), id)
	if err != nil {
		t.Fatalf("Allocate %s: %v", id, err)
	}
	t.Cleanup(func() {
		if err := s.Release(context.Background(), id); err != nil {
			t.Logf("release %s: %v", id, err)
		}
	})

	return spec
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()

	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}

	return string(out)
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(blob)
}

func requireNetworkTools(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("the network service needs root")
	}
	for _, binary := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("no %s on this host", binary)
		}
	}
}
