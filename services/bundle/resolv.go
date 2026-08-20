package bundle

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

const (
	etcDirPerm  = 0o755
	etcFilePerm = 0o644
)

// writeNetworkFiles puts the resolver configuration in the sandbox's own writable layer, where it
// shadows whatever the image shipped. Neither gVisor's netstack nor Firecracker's guest resolves a
// name; the guest's libc does, and it reads these two files.
func writeNetworkFiles(b Bundle, spec models.SandboxSpec) error {
	if !spec.Network.Address.IsValid() {
		return nil
	}

	etc := filepath.Join(b.Upper, "etc")
	if err := os.MkdirAll(etc, etcDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", etc, err)
	}

	files := map[string]string{
		"resolv.conf": resolvConf(spec.Network.Nameservers),
		"hosts":       hostsFile(spec),
	}

	for name, content := range files {
		path := filepath.Join(etc, name)
		if err := store.WriteFile(path, []byte(content), etcFilePerm); err != nil { // #nosec G306
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	return nil
}

// resolvConf lists the nameservers the network service allocated. A sandbox with none resolves no
// name, which is a working sandbox with no DNS rather than one that hangs on a dead resolver.
func resolvConf(nameservers []netip.Addr) string {
	var out strings.Builder

	for _, server := range nameservers {
		fmt.Fprintf(&out, "nameserver %s\n", server)
	}

	return out.String()
}

// hostsFile gives the sandbox its own name, which many programs expect to resolve.
func hostsFile(spec models.SandboxSpec) string {
	hostname := firstNonEmpty(spec.Name, spec.ID)

	return fmt.Sprintf("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n%s\t%s\n",
		spec.Network.Address.Addr(), hostname)
}
