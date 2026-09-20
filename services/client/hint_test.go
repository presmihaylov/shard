package client

import "testing"

// The unit serves the default root only, so that root is the one the hint may send the operator to it for, and the unit is the host's.
func TestHintNamesTheUnitForTheDefaultRootOnly(t *testing.T) {
	if got := hintFor(DefaultRoot, "linux"); got != "systemctl status shard" {
		t.Errorf("the default root hints %q on linux", got)
	}
	if got := hintFor(DefaultRoot, "darwin"); got != "launchctl print system/shard.daemon" {
		t.Errorf("the default root hints %q on darwin", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := hintFor("/srv/shard-e2e", goos); got != "shard --root /srv/shard-e2e daemon" {
			t.Errorf("another root hints %q on %s", got, goos)
		}
	}
}
