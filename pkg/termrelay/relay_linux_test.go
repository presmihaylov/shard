//go:build linux

package termrelay

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/launch"
)

// The relay's record and the host's parse of it are two halves of one wire, so one test holds them together.
func TestARecordTheRelayWritesIsTheErrnoTheHostReads(t *testing.T) {
	cases := []struct {
		kind  byte
		errno unix.Errno
	}{{failed, unix.ENOENT}, {failed, unix.ENOEXEC}, {unentered, unix.EACCES}}

	for _, c := range cases {
		err := Await(bytes.NewReader(record(c.kind, c.errno)), func() { t.Errorf("%c%d ran onStart", c.kind, c.errno) })

		got, ok := errors.AsType[*launch.NotStartedError](err)
		if !ok || got.Errno != c.errno || got.Chdir != (c.kind == unentered) {
			t.Errorf("%c%d: Await returned %v", c.kind, c.errno, err)
		}
	}
}
