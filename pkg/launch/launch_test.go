package launch

import (
	"syscall"
	"testing"
)

func TestAWorkDirectoryRefusalNamesTheDirectory(t *testing.T) {
	for errno, want := range map[syscall.Errno]string{
		syscall.ENOENT:  `the work directory "/work" does not exist`,
		syscall.ENOTDIR: `the work directory "/work" is not a directory`,
		syscall.EACCES:  `the work directory "/work" cannot be entered: permission denied`,
	} {
		if got := WorkDirReason("/work", errno); got != want {
			t.Errorf("WorkDirReason(%v) = %q, want %q", errno, got, want)
		}
	}
}
