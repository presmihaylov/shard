package mountinfo

import (
	"strings"
	"testing"
)

const sample = `22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro
40 22 0:35 / /var/lib/shard/sandboxes/s1/disk rw,relatime shared:20 - ext4 /dev/loop3 rw
41 40 0:36 / /var/lib/shard/sandboxes/s1/bundle/rootfs rw,relatime shared:21 master:3 - overlay overlay rw,lowerdir=/var/lib/shard/images/a,upperdir=/var/lib/shard/sandboxes/s1/disk/upper,workdir=/var/lib/shard/sandboxes/s1/disk/work
42 22 0:37 / /var/lib/shard/sandboxes/s1/disk rw,relatime shared:22 - tmpfs tmpfs rw
43 22 0:38 / /mnt/with\040space rw,relatime - ext4 /dev/loop4 rw
`

func TestAtReportsTheLastMountAtAPoint(t *testing.T) {
	m, found, err := parse(strings.NewReader(sample), "/var/lib/shard/sandboxes/s1/disk")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found {
		t.Fatal("the disk mount was not found")
	}

	// The tmpfs was mounted over the loop later, so it is what a process at the point sees.
	if m.FSType != "tmpfs" || m.Source != "tmpfs" {
		t.Errorf("got %+v, want the tmpfs that shadows the loop", m)
	}
}

func TestAtReadsTheOptionalFieldsPastTheSeparator(t *testing.T) {
	m, found, err := parse(strings.NewReader(sample), "/var/lib/shard/sandboxes/s1/bundle/rootfs")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found {
		t.Fatal("the overlay was not found")
	}

	if m.FSType != "overlay" || !strings.Contains(m.SuperOptions, "upperdir=/var/lib/shard/sandboxes/s1/disk/upper") {
		t.Errorf("got %+v, want the overlay and its upperdir", m)
	}
}

func TestAtUnescapesThePoint(t *testing.T) {
	m, found, err := parse(strings.NewReader(sample), "/mnt/with space")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !found || m.Source != "/dev/loop4" {
		t.Errorf("got %+v, found %v, want the loop at the escaped point", m, found)
	}
}

func TestAtReportsNothingForAnUnmountedPoint(t *testing.T) {
	_, found, err := parse(strings.NewReader(sample), "/var/lib/shard/sandboxes/s2/disk")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if found {
		t.Error("an unmounted point was reported as mounted")
	}
}
