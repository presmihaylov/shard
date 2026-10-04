package bundle

import (
	"errors"
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

	files := map[string]string{
		"resolv.conf": resolvConf(spec.Network.Nameservers),
		"hosts":       hostsFile(spec),
	}

	// No container holds the layer yet, so sysbox-runc has not shifted it.
	for name, content := range files {
		if err := writeLayer(b.Upper, filepath.Join("etc", name), []byte(content), idShift{}); err != nil {
			return err
		}
	}

	return nil
}

// writeLayer writes name under layer, the upper layer or the merged view over it, which the guest writes too: a guest symlink that leads out of it is refused, never followed onto the host.
func writeLayer(layer, name string, data []byte, shift idShift) error {
	root, err := os.OpenRoot(layer)
	if err != nil {
		return fmt.Errorf("open the writable layer %s: %w", layer, err)
	}

	if err := root.MkdirAll(filepath.Dir(name), etcDirPerm); err != nil {
		return errors.Join(fmt.Errorf("create %s in the writable layer %s: %w", filepath.Dir(name), layer, err), root.Close())
	}
	if err := store.WriteFileIn(root, name, data, etcFilePerm); err != nil { // #nosec G306
		return errors.Join(fmt.Errorf("write %s in the writable layer %s: %w", name, layer, err), root.Close())
	}
	if err := shift.chownIn(root, name); err != nil {
		return errors.Join(err, root.Close())
	}

	return root.Close()
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
