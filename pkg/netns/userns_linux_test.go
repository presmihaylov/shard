//go:build linux

package netns

import (
	"syscall"
	"testing"
)

// The holder's namespace must keep setgroups allowed, or every guest that drops to a user dies with EPERM (SHARD-211).
func TestTheHolderKeepsSetgroupsAllowed(t *testing.T) {
	attr := holderAttr(IDMapping{HostID: 165536, Size: 65536})

	if !attr.GidMappingsEnableSetgroups {
		t.Error("the holder is born with setgroups denied, which is final for the namespace")
	}
	if attr.Cloneflags&syscall.CLONE_NEWUSER == 0 || attr.Cloneflags&syscall.CLONE_NEWNET == 0 {
		t.Errorf("the holder clones with %#x, want a new user and network namespace", attr.Cloneflags)
	}

	want := syscall.SysProcIDMap{ContainerID: 0, HostID: 165536, Size: 65536}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0] != want {
		t.Errorf("uid mapping is %v, want %v", attr.UidMappings, want)
	}
	if len(attr.GidMappings) != 1 || attr.GidMappings[0] != want {
		t.Errorf("gid mapping is %v, want %v", attr.GidMappings, want)
	}
}
