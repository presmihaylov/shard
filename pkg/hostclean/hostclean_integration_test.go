//go:build integration

package hostclean

import (
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

func names(left []Leftover, what string) bool {
	return slices.ContainsFunc(left, func(l Leftover) bool { return l.What == what })
}
