package supervisor_test

import (
	"net/netip"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

const (
	blockBegin = "# BEGIN shard links: shard rewrites the lines up to the END line as the links change\n"
	blockEnd   = "# END shard links\n"
	guestLines = "127.0.0.1\tlocalhost\n10.200.0.2\tsb-1\n"
)

var linked = []models.Host{
	{Address: netip.MustParseAddr("10.200.0.3"), Names: []string{"my_db", "my-db"}},
	{Address: netip.MustParseAddr("10.200.0.4"), Names: []string{"cache"}},
}

const linkedBlock = blockBegin + "10.200.0.3\tmy_db my-db\n10.200.0.4\tcache\n" + blockEnd

func TestWithHosts(t *testing.T) {
	cases := []struct {
		name    string
		hosts   string
		entries []models.Host
		want    string
	}{
		{"appends the block", guestLines, linked, guestLines + linkedBlock},
		{"ends the last line before the block", "127.0.0.1\tlocalhost", linked, "127.0.0.1\tlocalhost\n" + linkedBlock},
		{"writes into an empty file", "", linked, linkedBlock},
		{
			"replaces only its own block, where it stood",
			guestLines + blockBegin + "10.200.0.9\told\n" + blockEnd + "10.0.0.1\tmine\n",
			linked[1:],
			guestLines + blockBegin + "10.200.0.4\tcache\n" + blockEnd + "10.0.0.1\tmine\n",
		},
		{"takes the block out with no entries", guestLines + linkedBlock + "10.0.0.1\tmine\n", nil, guestLines + "10.0.0.1\tmine\n"},
		{"leaves a file with no block as it is", guestLines, nil, guestLines},
		{"takes a block with no end to the end of the file", guestLines + blockBegin + "10.200.0.9\told\n", linked[1:], guestLines + blockBegin + "10.200.0.4\tcache\n" + blockEnd},
		{"folds two blocks into one", blockBegin + blockEnd + guestLines + blockBegin + blockEnd, linked[1:], blockBegin + "10.200.0.4\tcache\n" + blockEnd + guestLines},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(supervisor.WithHosts([]byte(c.hosts), c.entries))
			if got != c.want {
				t.Fatalf("WithHosts(%q) = %q, want %q", c.hosts, got, c.want)
			}
			if again := string(supervisor.WithHosts([]byte(got), c.entries)); again != got {
				t.Fatalf("a second write changed %q to %q", got, again)
			}
		})
	}
}
