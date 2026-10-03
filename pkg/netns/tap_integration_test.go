//go:build integration

package netns_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/netns"
)

// The tun driver looks a tap up in the namespace it is opened in, so the owner must land on the tap there and leave no tap on the host.
func TestChownTapInGivesTheTapInTheNamespaceItsOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("a tap needs root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip on this host")
	}

	m, err := netns.New()
	if err != nil {
		t.Fatalf("open the netns manager: %v", err)
	}

	const namespace, tap = "shardt-tap", "shardtt0"
	if err := m.AddNamespace(t.Context(), namespace); err != nil {
		t.Fatalf("AddNamespace: %v", err)
	}
	t.Cleanup(func() {
		if err := m.DeleteNamespace(context.Background(), namespace); err != nil {
			t.Logf("remove the namespace %s: %v", namespace, err)
		}
	})
	if err := m.AddTapIn(t.Context(), namespace, tap); err != nil {
		t.Fatalf("AddTapIn: %v", err)
	}

	// Ids with no passwd or group entry, so ip prints the numbers.
	if err := netns.ChownTapIn(namespace, tap, 4242, 4343); err != nil {
		t.Fatalf("ChownTapIn: %v", err)
	}

	out, err := exec.Command("ip", "-netns", namespace, "-details", "link", "show", tap).CombinedOutput()
	if err != nil {
		t.Fatalf("ip link show %s in %s: %v: %s", tap, namespace, err, out)
	}
	for _, want := range []string{"tun type tap", "user 4242", "group 4343"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the tap %s is not %q: %q", tap, want, strings.TrimSpace(string(out)))
		}
	}

	exists, err := m.LinkExists(t.Context(), tap)
	if err != nil {
		t.Fatalf("LinkExists: %v", err)
	}
	if exists {
		t.Errorf("ChownTapIn left a link %s on the host", tap)
	}
}
