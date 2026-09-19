package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/supervisor"
)

// writeResolverFilesIn replaces each file by rename, as an image often ships resolv.conf as a symlink into a /run nothing here mounts.
func writeResolverFilesIn(etc string, a supervisor.Address) error {
	if err := os.MkdirAll(etc, 0o755); err != nil { //nolint:gosec // every process reads /etc
		return fmt.Errorf("create %s: %w", etc, err)
	}
	var resolv strings.Builder
	for _, server := range a.Nameservers {
		fmt.Fprintf(&resolv, "nameserver %s\n", server)
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if a.Hostname != "" {
		hosts += fmt.Sprintf("%s\t%s\n", a.IP, a.Hostname)
	}
	for name, content := range map[string]string{"resolv.conf": resolv.String(), "hosts": hosts} {
		if err := store.WriteFile(filepath.Join(etc, name), []byte(content), 0o644); err != nil { //nolint:gosec // libc reads them as every process
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}
