package client

import "testing"

// The unit serves the default root only, so that root is the one the hint may send the operator to it for.
func TestHintNamesTheUnitForTheDefaultRootOnly(t *testing.T) {
	if got := hint(DefaultRoot); got != "systemctl status shard" {
		t.Errorf("the default root hints %q", got)
	}
	if got := hint("/srv/shard-e2e"); got != "shard --root /srv/shard-e2e daemon" {
		t.Errorf("another root hints %q", got)
	}
}
