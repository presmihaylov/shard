package main

import (
	"bytes"
	"os"
	"testing"
)

// A /proc file states a size of 0 and a sysfs one 4096, so a get that stopped at the stat would answer an empty file or fail.
func TestFilesGetOfAKernelFileStreamsWhatItReads(t *testing.T) {
	for _, path := range []string{"/proc/self/status", "/proc/meminfo", "/sys/devices/system/cpu/online"} {
		t.Run(path, func(t *testing.T) {
			if _, err := os.Stat(path); err != nil {
				t.Skipf("this host has no %s: %v", path, err)
			}
			open := startFiles(t)

			stat, got, err := getFile(t, open, path)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if len(got) == 0 || int64(len(got)) == stat.Size || bytes.IndexByte(got, '\n') < 0 {
				t.Fatalf("get gave %q with a stat size of %d, want the lines the kernel made, whatever the size says", got, stat.Size)
			}
		})
	}
}
