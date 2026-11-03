package bundle

import (
	"net/netip"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestHostsFileHoldsTheLinkedHostsAfterItsOwnName(t *testing.T) {
	spec := models.SandboxSpec{ID: "sb-1", Network: models.NetworkSpec{
		Address: netip.MustParsePrefix("10.200.0.2/24"),
		Hosts:   []models.Host{{Address: netip.MustParseAddr("10.200.0.3"), Names: []string{"cache"}}},
	}}

	want := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.200.0.2\tsb-1\n" +
		"# BEGIN shard links: shard rewrites the lines up to the END line as the links change\n10.200.0.3\tcache\n# END shard links\n"
	if got := hostsFile(spec); got != want {
		t.Fatalf("hostsFile = %q, want %q", got, want)
	}
}
