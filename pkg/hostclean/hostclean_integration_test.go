//go:build integration

package hostclean

import (
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

func TestLinkPatternNamesEveryHostVeth(t *testing.T) {
	listed := "2: eth0: <BROADCAST> mtu 1500\n3: shardv2@if4: <BROADCAST> mtu 1500\n4: shard0: <BROADCAST> mtu 1500\n5: shardv2@if9: <UP>\n"

	if got := unique(linkPattern.FindAllString(listed, -1)); !slices.Equal(got, []string{"shardv2"}) {
		t.Errorf("the links = %v, want the veth once and not the bridge", got)
	}
}
