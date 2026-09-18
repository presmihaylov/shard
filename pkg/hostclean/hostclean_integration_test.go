//go:build integration

package hostclean

import (
	"os"
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

// The runtime's state under the root is what names a sandbox it still holds, and only then is it a leftover.
func TestSandboxOfNamesTheSandboxTheRuntimeHolds(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, sandboxDir, "amber-otter-1a2b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, recordFile), []byte(`{"provider":"gvisor"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if left := sandboxOf(root, "amber-otter-1a2b"); names(left, "the sandbox") {
		t.Errorf("runsc holds nothing under %s, yet the sandbox is named: %v", root, left)
	}

	if err := os.MkdirAll(filepath.Join(root, "runsc", "amber-otter-1a2b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if left := sandboxOf(root, "amber-otter-1a2b"); !names(left, "the sandbox") {
		t.Errorf("runsc holds the sandbox under %s, yet it is not named: %v", root, left)
	}
}

func names(left []Leftover, what string) bool {
	return slices.ContainsFunc(left, func(l Leftover) bool { return l.What == what })
}
