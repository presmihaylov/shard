package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestGuestBootIsOneDiskOrTwo(t *testing.T) {
	cases := map[string]struct {
		boot guestBoot
		set  bool
		ok   bool
	}{
		"nothing":          {guestBoot{}, false, true},
		"one disk":         {guestBoot{Root: "/dev/vda"}, true, true},
		"base and overlay": {guestBoot{Base: "/dev/vda", Overlay: "/dev/vdb"}, true, true},
		"root with a base": {guestBoot{Root: "/dev/vda", Base: "/dev/vdb", Overlay: "/dev/vdc"}, true, false},
		"base alone":       {guestBoot{Base: "/dev/vda"}, true, false},
		"overlay alone":    {guestBoot{Overlay: "/dev/vdb"}, false, false},
		"reboot on a disk": {guestBoot{Root: "/dev/vda", Reboot: true}, true, true},
		"reboot alone":     {guestBoot{Reboot: true}, false, false},
		"swap on a disk":   {guestBoot{Root: "/dev/vda", SwapMiB: 2048}, true, true},
		"swap on overlay":  {guestBoot{Base: "/dev/vda", Overlay: "/dev/vdb", SwapMiB: 2048}, true, true},
		"swap alone":       {guestBoot{SwapMiB: 2048}, false, false},
		"negative swap":    {guestBoot{Root: "/dev/vda", SwapMiB: -1}, true, false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.boot.set(); got != c.set {
				t.Errorf("set() = %v, want %v", got, c.set)
			}
			if err := c.boot.check(); (err == nil) != c.ok {
				t.Errorf("check() = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// The kernel's swapon reads the version and last_page at 1024 and the magic at the end of the first page, as mkswap lays them.
func TestSwapHeaderIsTheOneMkswapWrites(t *testing.T) {
	const pageSize = 4096
	header, err := swapHeader(2048<<20, pageSize)
	if err != nil {
		t.Fatalf("swapHeader: %v", err)
	}
	if len(header) != pageSize {
		t.Fatalf("the header is %d bytes, want one page of %d", len(header), pageSize)
	}
	if version := binary.NativeEndian.Uint32(header[1024:]); version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if last := binary.NativeEndian.Uint32(header[1028:]); last != 2048<<8-1 {
		t.Errorf("last_page = %d, want %d", last, 2048<<8-1)
	}
	if magic := header[pageSize-10:]; !bytes.Equal(magic, []byte("SWAPSPACE2")) {
		t.Errorf("the magic is %q, want SWAPSPACE2", magic)
	}
	if !bytes.Equal(header[:1024], make([]byte, 1024)) {
		t.Error("the boot block before the header is not zero")
	}
}

func TestSwapHeaderRefusesASizeTheKernelCannotTake(t *testing.T) {
	for name, size := range map[string]int64{"one page": 4096, "no page": 0, "past 2^32 pages": (1<<32 + 1) * 4096} {
		if _, err := swapHeader(size, 4096); err == nil {
			t.Errorf("swapHeader(%s) took it", name)
		}
	}
}

func TestLinkStdioLaysTheLinksARuntimeMakes(t *testing.T) {
	dev := t.TempDir()
	if err := linkStdio(dev); err != nil {
		t.Fatalf("linkStdio: %v", err)
	}

	for name, want := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		got, err := os.Readlink(filepath.Join(dev, name))
		if err != nil {
			t.Errorf("read /dev/%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("/dev/%s links to %q, want %q", name, got, want)
		}
	}
}
