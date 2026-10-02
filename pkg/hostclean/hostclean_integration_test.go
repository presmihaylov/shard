//go:build integration

package hostclean

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// The mounts come back deepest first, because an overlay pins the root it lives under.
func TestMountPointsReadsTheOnesUnderARootOfOurs(t *testing.T) {
	listed := "24 1 0:22 / /proc rw shared:5 - proc proc rw\n" +
		"31 24 0:28 / /tmp/shard-itest99/sandboxes/one/bundle rw - overlay overlay rw\n" +
		"32 24 0:29 / /tmp/shard-other/mnt rw - overlay overlay rw\n"

	points := mountPoints(listed, []string{"/tmp/shard-itest"})
	if want := []string{"/tmp/shard-itest99/sandboxes/one/bundle"}; !slices.Equal(points, want) {
		t.Errorf("mountPoints = %v, want %v", points, want)
	}
}

// The record is the only handle on the veth and on who holds the sandbox, so an unfinished record names neither.
func TestReadRecordReadsTheHandlesOffTheRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"id":"amber-otter-1a2b","provider":"gvisor","host_interface":"shardv7"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := readRecord(path); got != (record{Provider: "gvisor", HostInterface: "shardv7"}) {
		t.Errorf("readRecord = %+v, want gvisor and shardv7", got)
	}
	if got := readRecord(filepath.Join(dir, "missing.json")); got != (record{}) {
		t.Errorf("a record that is not there named %+v", got)
	}
}

// The record naming a provider is what names a sandbox to delete: runsc keeps no <root>/runsc/<id>, so a stat there let a live sentry outlive a killed daemon (SHARD-226).
func TestSandboxOfNamesTheSandboxTheRecordHolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
		named  bool
	}{
		{"gvisor, with no runtime dir", `{"provider":"gvisor"}`, true},
		{"sysbox", `{"provider":"sysbox"}`, true},
		{"runc", `{"provider":"runc"}`, true},
		{"a record a crashed run never finished", `{"provider":`, false},
		{"a provider this package does not drive", `{"provider":"firecracker"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, sandboxDir, "amber-otter-1a2b")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, recordFile), []byte(tc.record), 0o600); err != nil {
				t.Fatal(err)
			}

			if left := sandboxOf(root, "amber-otter-1a2b"); names(left, "the sandbox") != tc.named {
				t.Errorf("the record %s names the sandbox = %v, want %v: %v", tc.record, !tc.named, tc.named, left)
			}
		})
	}
}

// The fix above rests on this: every runtime takes a forced delete of a sandbox it does not hold as done.
func TestAForcedDeleteOfASandboxTheRuntimeDoesNotHoldIsDone(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the runtimes want root")
	}

	for provider, binary := range runtimes {
		t.Run(provider, func(t *testing.T) {
			if _, err := exec.LookPath(binary); err != nil {
				t.Skipf("no %s on PATH", binary)
			}

			state := filepath.Join(t.TempDir(), binary)
			if err := run(binary, "--root", state, "delete", "--force", "amber-otter-1a2b")(); err != nil {
				t.Errorf("%s refused to delete a sandbox it does not hold: %v", binary, err)
			}
		})
	}
}

// The namespace delete takes the veth pair with it, so the link delete after it finds nothing and Sweep must still come back clean (SHARD-227).
func TestALinkThatIsGoneIsSwept(t *testing.T) {
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip on PATH")
	}

	if err := deleteLink("shardv-none")(); err != nil {
		t.Errorf("a link that is gone was not taken as swept: %v", err)
	}
}

// A link that is there is still taken, and a second take of it is the no-op the first left.
func TestALinkThatIsThereIsTaken(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ip link add wants root")
	}

	const name = "shardvt227"
	if err := run("ip", "link", "add", name, "type", "veth", "peer", "name", name+"p")(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deleteLink(name)(); err != nil {
			t.Error(err)
		}
	})

	for range 2 {
		if err := deleteLink(name)(); err != nil {
			t.Fatalf("deleteLink %s: %v", name, err)
		}
		if shown(name) {
			t.Fatalf("%s is still on the host", name)
		}
	}
}

// SHARD-272: every root shares the bridge and the tables, so a port on the bridge keeps them, and once it goes they go.
func TestSweepTakesTheHostNetOnlyOnceNothingHoldsIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ip link and nft want root")
	}
	for _, binary := range []string{"ip", "nft", "ss"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("no %s on PATH", binary)
		}
	}
	held, err := hostNetHeld()
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Skip("a sandbox or a daemon on this host holds the bridge and the tables")
	}

	const port = "shardvt272"
	if !shown(hostBridge) {
		mustRun(t, "ip", "link", "add", hostBridge, "type", "bridge")
	}
	for _, family := range hostTableFamilies {
		mustRun(t, "nft", "add", "table", family, hostTable)
	}
	mustRun(t, "ip", "link", "add", port, "type", "veth", "peer", "name", port+"p")
	t.Cleanup(func() {
		if err := errors.Join(deleteLink(port)(), sweepHostNet()); err != nil {
			t.Error(err)
		}
	})
	mustRun(t, "ip", "link", "set", port, "master", hostBridge)

	if err := sweepHostNet(); err != nil {
		t.Fatalf("sweep with a port on the bridge: %v", err)
	}
	if !shown(hostBridge) || tablesListed(t) != len(hostTableFamilies) {
		t.Fatalf("the sweep took the bridge or a table while %s was a port of %s", port, hostBridge)
	}

	if err := deleteLink(port)(); err != nil {
		t.Fatal(err)
	}
	if err := sweepHostNet(); err != nil {
		t.Fatalf("sweep with no port on the bridge: %v", err)
	}
	if shown(hostBridge) || tablesListed(t) != 0 {
		t.Errorf("the bridge or a table outlived a sweep that nothing held")
	}
}

// tablesListed counts the shard tables the host lists.
func tablesListed(t *testing.T) int {
	t.Helper()

	count := 0
	for _, family := range hostTableFamilies {
		if exec.Command("nft", "list", "table", family, hostTable).Run() == nil {
			count++
		}
	}

	return count
}

func mustRun(t *testing.T, binary string, args ...string) {
	t.Helper()

	if err := run(binary, args...)(); err != nil {
		t.Fatal(err)
	}
}

func names(left []Leftover, what string) bool {
	return slices.ContainsFunc(left, func(l Leftover) bool { return l.What == what })
}
