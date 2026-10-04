package bundle

import (
	"os"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var sysboxMapping = []specs.LinuxIDMapping{{ContainerID: 0, HostID: 165536, Size: 65536}}

func TestHostIDShiftsOnlyAGuestIDTheNamespaceMaps(t *testing.T) {
	cases := map[string]struct {
		id, want uint32
		mappings []specs.LinuxIDMapping
	}{
		"guest root":                 {id: 0, want: 165536, mappings: sysboxMapping},
		"a guest user":               {id: 100, want: 165636, mappings: sysboxMapping},
		"the last guest id":          {id: 65535, want: 231071, mappings: sysboxMapping},
		"a host id of the namespace": {id: 165541, want: 165541, mappings: sysboxMapping},
		"an id the namespace lacks":  {id: 65536, want: 65536, mappings: sysboxMapping},
		"no namespace":               {id: 0, want: 0},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := hostID(c.id, c.mappings); got != c.want {
				t.Errorf("hostID(%d) = %d, want %d", c.id, got, c.want)
			}
		})
	}
}

func TestLayerShiftFollowsTheOwnerOfTheUpperLayer(t *testing.T) {
	b := Bundle{Upper: t.TempDir()}

	info, err := os.Lstat(b.Upper)
	if err != nil {
		t.Fatalf("stat the upper layer: %v", err)
	}
	uid, _, err := owner(info)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}

	cases := map[string]struct {
		linux   *specs.Linux
		shifted bool
	}{
		"no user namespace":                  {linux: &specs.Linux{}},
		"no linux section":                   {},
		"a layer chowned into the namespace": {linux: &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{HostID: uid, Size: 1}}}, shifted: true},
		"a layer chowned back out of it":     {linux: &specs.Linux{UIDMappings: []specs.LinuxIDMapping{{HostID: uid + 1, Size: 1}}}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			shift, err := b.layerShift(&specs.Spec{Linux: c.linux})
			if err != nil {
				t.Fatalf("layerShift: %v", err)
			}
			if got := len(shift.uids) > 0; got != c.shifted {
				t.Errorf("shifted = %t, want %t", got, c.shifted)
			}
		})
	}
}
