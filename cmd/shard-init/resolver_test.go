package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

// An image's resolv.conf is often a dangling symlink into /run; the guest replaces it with a regular file of its own.
func TestResolverFilesReplaceADanglingSymlink(t *testing.T) {
	etc := t.TempDir()
	if err := os.Symlink("/run/systemd/resolve/stub-resolv.conf", filepath.Join(etc, "resolv.conf")); err != nil {
		t.Fatal(err)
	}

	err := writeResolverFilesIn(etc, supervisor.Address{IP: "10.200.0.2", Hostname: "sb-1", Nameservers: []string{"10.200.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(etc, "resolv.conf"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("resolv.conf is %v, %v; want a regular file", info.Mode(), err)
	}
	resolv, _ := os.ReadFile(filepath.Join(etc, "resolv.conf"))
	if string(resolv) != "nameserver 10.200.0.1\n" {
		t.Fatalf("resolv.conf = %q", resolv)
	}
	hosts, _ := os.ReadFile(filepath.Join(etc, "hosts"))
	if string(hosts) != "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.200.0.2\tsb-1\n" {
		t.Fatalf("hosts = %q", hosts)
	}
	if _, err := os.Stat("/run/systemd/resolve/stub-resolv.conf"); err == nil {
		t.Fatal("the write followed the symlink")
	}
}
