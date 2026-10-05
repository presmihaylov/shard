package bundle

import (
	"os"
	"testing"

	"github.com/presmihaylov/shard/pkg/netns"
)

var sysboxShift = idShift{HostID: 165536, Size: 65536}

func TestHostIDShiftsOnlyAGuestIDTheNamespaceMaps(t *testing.T) {
	cases := map[string]struct {
		id, want uint32
		shift    idShift
	}{
		"guest root":                 {id: 0, want: 165536, shift: sysboxShift},
		"a guest user":               {id: 100, want: 165636, shift: sysboxShift},
		"the last guest id":          {id: 65535, want: 231071, shift: sysboxShift},
		"a host id of the namespace": {id: 165541, want: 165541, shift: sysboxShift},
		"an id the namespace lacks":  {id: 65536, want: 65536, shift: sysboxShift},
		"no namespace":               {id: 0, want: 0},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.shift.hostID(c.id); got != c.want {
				t.Errorf("hostID(%d) = %d, want %d", c.id, got, c.want)
			}
		})
	}
}

func TestLayerShiftFollowsTheOwnerOfTheUpperLayer(t *testing.T) {
	upper := t.TempDir()

	info, err := os.Lstat(upper)
	if err != nil {
		t.Fatalf("stat the upper layer: %v", err)
	}
	uid, _, err := owner(info)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}

	cases := map[string]struct {
		userns  netns.IDMapping
		shifted bool
	}{
		"a substrate that never shifts":      {},
		"a layer chowned into the namespace": {userns: netns.IDMapping{HostID: uid, Size: 1}, shifted: true},
		"a layer chowned back out of it":     {userns: netns.IDMapping{HostID: uid + 1, Size: 1}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			shift, err := Bundle{Upper: upper, Userns: c.userns}.layerShift()
			if err != nil {
				t.Fatalf("layerShift: %v", err)
			}
			if got := shift.Size != 0; got != c.shifted {
				t.Errorf("shifted = %t, want %t", got, c.shifted)
			}
		})
	}
}
