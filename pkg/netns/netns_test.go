package netns

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fake stands in for ip: it records the argv it was called with, then prints what a test asked for.
func fake(t *testing.T, stdout, stderr string, exitCode int) (*Manager, string) {
	t.Helper()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	binary := filepath.Join(dir, "ip")

	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"printf '%s' '" + stdout + "'\n" +
		"printf '%s' '" + stderr + "' >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"

	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake ip: %v", err)
	}

	return &Manager{ip: binary, nft: binary}, argvFile
}

// fakeFailing stands in for ip when only some of a verb's calls must fail: it fails the ones whose
// argv contains needle, and succeeds for the rest.
func fakeFailing(t *testing.T, needle, stderr string, exitCode int) *Manager {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "ip")

	script := "#!/bin/sh\ncase \" $* \" in\n  *' " + needle + " '*)\n    printf '%s' '" + stderr +
		"' >&2\n    exit " + strconv.Itoa(exitCode) + "\n    ;;\nesac\nexit 0\n"

	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake ip: %v", err)
	}

	return &Manager{ip: binary, nft: binary}
}

func argv(t *testing.T, path string) []string {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake ip recorded no argv: %v", err)
	}

	return strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n")
}

func TestNewRefusesAHostThatIsNotLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this host is Linux, so New is expected to succeed")
	}

	_, err := New()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported off Linux", err)
	}
}

func TestNamespacePathIsWhereIproute2BindsIt(t *testing.T) {
	if got := NamespacePath("amber-otter-1a2b"); got != "/var/run/netns/amber-otter-1a2b" {
		t.Errorf("got %q, want the path under %s", got, RunDir)
	}
}

// The driver refuses an object that is already there rather than adopting it: only the caller holding
// the lease knows whether what a crashed run left behind is its own.
func TestAddNamespaceReportsOneThatExists(t *testing.T) {
	m, _ := fake(t, "", "Cannot create namespace file \"/var/run/netns/x\": File exists", 1)

	if err := m.AddNamespace(t.Context(), "x"); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
}

func TestDeleteNamespaceAcceptsOneThatIsGone(t *testing.T) {
	m, _ := fake(t, "", "Cannot remove namespace file \"/var/run/netns/x\": No such file or directory", 1)

	if err := m.DeleteNamespace(t.Context(), "x"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
}

func TestDeleteLinkAcceptsOneThatIsGone(t *testing.T) {
	m, _ := fake(t, "", "Cannot find device \"shardv2\"", 1)

	if err := m.DeleteLink(t.Context(), "shardv2"); err != nil {
		t.Fatalf("DeleteLink: %v", err)
	}
}

func TestAddVethReportsAPairThatExists(t *testing.T) {
	m, _ := fake(t, "", "RTNETLINK answers: File exists", 2)

	if err := m.AddVeth(t.Context(), "shardv2", "eth0", "x"); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
}

// The bridge is shared by every sandbox, so the second shard process to start must not fail on it.
func TestEnsureBridgeAcceptsABridgeThatExists(t *testing.T) {
	m := fakeFailing(t, "add", "RTNETLINK answers: File exists", 2)

	if err := m.EnsureBridge(t.Context(), "shard0", netip.MustParsePrefix("10.87.0.1/16")); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}
}

// Everything else must still surface, or a broken host would look like an idempotent call.
func TestAFailureThatIsNeitherReachesTheCaller(t *testing.T) {
	m, _ := fake(t, "", "RTNETLINK answers: Operation not permitted", 2)

	err := m.SetUp(t.Context(), "shardv2")
	if err == nil {
		t.Fatal("SetUp hid a failure")
	}
	if errors.Is(err, ErrExists) || errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want neither ErrExists nor ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("got %v, want the message ip printed", err)
	}
}

func TestLinkExistsReportsAMissingLink(t *testing.T) {
	m, _ := fake(t, "", "Device \"shardv2\" does not exist.", 1)

	exists, err := m.LinkExists(t.Context(), "shardv2")
	if err != nil {
		t.Fatalf("LinkExists: %v", err)
	}
	if exists {
		t.Error("LinkExists reported a link ip cannot find")
	}
}

// ip addr has its own word for a duplicate, and EnsureBridge is only idempotent because it is matched.
func TestAnAddressThatIsAlreadyAssignedIsReportedAsExisting(t *testing.T) {
	m, _ := fake(t, "", "Error: ipv4: Address already assigned.", 2)

	err := m.AddAddressIn(t.Context(), "amber-otter", "eth0", netip.MustParsePrefix("10.87.0.2/16"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
}

// The isolated flag is what keeps two sandboxes on one bridge apart, so the argv must carry it.
func TestIsolatePortAsksForTheBridgeSlaveFlag(t *testing.T) {
	m, argvFile := fake(t, "", "", 0)

	if err := m.IsolatePort(t.Context(), "shardv2"); err != nil {
		t.Fatalf("IsolatePort: %v", err)
	}

	want := []string{"link", "set", "dev", "shardv2", "type", "bridge_slave", "isolated", "on"}
	if got := argv(t, argvFile); !slices.Equal(got, want) {
		t.Errorf("got argv %v, want %v", got, want)
	}
}

func TestAddDefaultRouteInRunsInsideTheNamespace(t *testing.T) {
	m, argvFile := fake(t, "", "", 0)

	if err := m.AddDefaultRouteIn(t.Context(), "amber-otter", "eth0", netip.MustParseAddr("10.87.0.1")); err != nil {
		t.Fatalf("AddDefaultRouteIn: %v", err)
	}

	want := []string{"-netns", "amber-otter", "route", "add", "default", "via", "10.87.0.1", "dev", "eth0"}
	if got := argv(t, argvFile); !slices.Equal(got, want) {
		t.Errorf("got argv %v, want %v", got, want)
	}
}

func TestAddressesInReadsTheBriefForm(t *testing.T) {
	m, _ := fake(t, "eth0             UP             10.87.0.2/16 fe80::1/64 ", "", 0)

	got, err := m.AddressesIn(t.Context(), "amber-otter", "eth0")
	if err != nil {
		t.Fatalf("AddressesIn: %v", err)
	}

	want := []netip.Prefix{netip.MustParsePrefix("10.87.0.2/16"), netip.MustParsePrefix("fe80::1/64")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}
