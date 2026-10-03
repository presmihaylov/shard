package supervisor_test

import (
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

func TestOneLineKeepsAReasonToOneSafeBoundedLine(t *testing.T) {
	for _, c := range []struct {
		name, reason, want string
	}{
		{name: "plain", reason: "mount /dev/vdb: read-only file system", want: "mount /dev/vdb: read-only file system"},
		{name: "control bytes", reason: "mount\nfailed\x1b[31m\x7f\u0085", want: "mount failed [31m  "},
		{name: "invalid UTF-8", reason: "bad \xff byte", want: "bad ? byte"},
		{name: "at the bound", reason: strings.Repeat("a", 256), want: strings.Repeat("a", 256)},
		{name: "a rune across the bound", reason: strings.Repeat("a", 255) + "é", want: strings.Repeat("a", 255)},
	} {
		if got := supervisor.OneLine(c.reason); got != c.want {
			t.Errorf("%s: OneLine(%q) = %q, want %q", c.name, c.reason, got, c.want)
		}
	}
}
