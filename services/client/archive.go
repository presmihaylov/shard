package client

import (
	"io"

	"github.com/presmihaylov/shard/pkg/tarball"
)

// The caps on a tar a sandbox sends the host, which is untrusted: past either one the copy out is refused, never cut short.
const (
	MaxArchiveBytes   = 64 << 30
	MaxArchiveEntries = 1 << 20
)

// PackDir writes the host path src as a tar whose top entry is name, which PutArchive takes.
func PackDir(w io.Writer, src, name string) error {
	return tarball.Pack(w, src, name)
}

// Owner is who a copy out run under sudo hands what it writes on the host to.
type Owner = tarball.Owner

// UnpackArchive lands a sandbox's tar under dst, with the entry strip at dst itself: it refuses any entry or link that leaves dst, a device node, setuid and setgid, and a tar past the caps. A non-nil owner takes what it makes.
func UnpackArchive(r io.Reader, dst, strip string, owner *Owner) error {
	return tarball.Unpack(r, dst, tarball.Options{Strip: strip, ConfineLinks: true, MaxBytes: MaxArchiveBytes, MaxEntries: MaxArchiveEntries, Owner: owner})
}
