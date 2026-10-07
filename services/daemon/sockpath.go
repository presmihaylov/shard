package daemon

import (
	"fmt"
	"path/filepath"
	"syscall"

	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// maxSocketPath is 107 on Linux and 103 on a Mac: Go and the firecracker binary both refuse a path that fills sun_path.
const maxSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// checkSocketPaths refuses at start a root whose longest socket path passes the limit, which a create would otherwise find (SHARD-358).
func checkSocketPaths(root, provider string) error {
	root = filepath.Clean(root)
	what, longest := api.SocketFile, filepath.Join(root, api.SocketFile)

	for _, path := range sandboxSockets(root, provider) {
		if len(path) > len(longest) {
			what, longest = "a sandbox's "+filepath.Base(path), path
		}
	}

	over := len(longest) - maxSocketPath
	if over <= 0 {
		return nil
	}

	return fmt.Errorf("the root %s is too long for %s: %s under it takes %d bytes, past the %d a unix socket path holds; use a root of at most %d bytes", root, provider, what, len(longest), maxSocketPath, len(root)-over)
}

// sandboxSockets names the sockets a provider binds for the sandbox with the longest id: a microVM's in its jail, a vz VM's in its state directory, gVisor's port forward one in the exec scratch.
func sandboxSockets(root, provider string) []string {
	dir := sandboxstate.LongestDir(root)
	switch provider {
	case firecracker.Name:
		return firecracker.JailSockets(filepath.Join(root, jailDir), filepath.Base(dir))
	case vzvm.Name:
		var paths []string
		for _, name := range vzvm.SocketFiles() {
			paths = append(paths, filepath.Join(dir, name))
		}

		return paths
	case gvisor.Name:
		return []string{filepath.Join(root, execDir, runsc.PortForwardSocket)}
	}

	return nil
}
