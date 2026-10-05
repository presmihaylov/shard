package filemode_test

import (
	"io/fs"
	"testing"

	"github.com/presmihaylov/shard/pkg/filemode"
)

func TestNameSaysWhatTheFileIs(t *testing.T) {
	for mode, want := range map[fs.FileMode]string{
		0o644:                             "regular file",
		fs.ModeDir | 0o755:                "directory",
		fs.ModeSymlink | 0o777:            "symbolic link",
		fs.ModeNamedPipe | 0o600:          "named pipe",
		fs.ModeSocket | 0o755:             "socket",
		fs.ModeDevice | fs.ModeCharDevice: "character device",
		fs.ModeDevice | 0o660:             "block device",
		fs.ModeIrregular:                  "file of an unknown type",
	} {
		if got := filemode.Name(mode); got != want {
			t.Errorf("Name(%v) = %q, want %q", mode, got, want)
		}
	}
}
