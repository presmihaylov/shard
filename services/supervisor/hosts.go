package supervisor

import (
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
)

// The markers fence the lines shard owns in a guest's /etc/hosts, so a refresh leaves every other line as the guest has it.
const (
	hostsBegin = "# BEGIN shard links: shard rewrites the lines up to the END line as the links change"
	hostsEnd   = "# END shard links"
)

// WithHosts answers hosts with shard's block holding one line per entry, in place of the block it held; no entries takes the block out.
func WithHosts(hosts []byte, entries []models.Host) []byte {
	var kept []string
	at, ours := -1, false
	for _, line := range strings.SplitAfter(string(hosts), "\n") {
		marker := strings.TrimSpace(line)
		if marker == hostsBegin && at < 0 {
			at = len(kept)
		}
		ours = ours || marker == hostsBegin
		if !ours && line != "" {
			kept = append(kept, line)
		}
		// A block with no end runs to the end of the file, which is how a write cut short leaves it.
		if marker == hostsEnd {
			ours = false
		}
	}
	if len(entries) == 0 {
		return []byte(strings.Join(kept, ""))
	}
	if at < 0 {
		at = len(kept)
	}
	if at > 0 && !strings.HasSuffix(kept[at-1], "\n") {
		kept[at-1] += "\n"
	}

	return []byte(strings.Join(slices.Insert(kept, at, hostsBlock(entries)), ""))
}

func hostsBlock(entries []models.Host) string {
	var b strings.Builder
	b.WriteString(hostsBegin + "\n")
	for _, entry := range entries {
		b.WriteString(entry.Address.String() + "\t" + strings.Join(entry.Names, " ") + "\n")
	}
	b.WriteString(hostsEnd + "\n")

	return b.String()
}
