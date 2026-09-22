package main

import "testing"

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
