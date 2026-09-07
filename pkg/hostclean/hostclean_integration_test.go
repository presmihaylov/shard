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

// The record is the only handle on the veth, so a record a crashed run never finished names none.
func TestHostInterfaceReadsTheVethOffTheRecord(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(record, []byte(`{"id":"amber-otter-1a2b","host_interface":"shardv7"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := hostInterface(record); got != "shardv7" {
		t.Errorf("hostInterface = %q, want shardv7", got)
	}
	if got := hostInterface(filepath.Join(dir, "missing.json")); got != "" {
		t.Errorf("a record that is not there named %q", got)
	}
}
