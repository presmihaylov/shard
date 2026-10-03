package daemon

import (
	"fmt"
	"path/filepath"
	"syscall"

	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// maxSocketPath is 107 on Linux and 103 on a Mac: Go and the firecracker binary both refuse a path that fills sun_path.
const maxSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// checkSocketPaths refuses at start a root whose longest socket path passes the limit, which a create would otherwise find (SHARD-358).
func checkSocketPaths(root, provider string) error {
	root = filepath.Clean(root)
	what, longest := api.SocketFile, filepath.Join(root, api.SocketFile)

	dir := sandboxstate.LongestDir(root)
	for _, name := range sandboxSockets(provider) {
		if path := filepath.Join(dir, name); len(path) > len(longest) {
			what, longest = "a sandbox's "+name, path
		}
	}

	over := len(longest) - maxSocketPath
	if over <= 0 {
		return nil
	}

	return fmt.Errorf("the root %s is too long for %s: %s under it takes %d bytes, past the %d a unix socket path holds; use a root of at most %d bytes", root, provider, what, len(longest), maxSocketPath, len(root)-over)
}

// sandboxSockets names the sockets a provider binds in a sandbox's state directory; the container substrates bind none there.
func sandboxSockets(provider string) []string {
	switch provider {
	case firecracker.Name:
		return firecracker.SocketFiles()
	case vzvm.Name:
		return vzvm.SocketFiles()
	}

	return nil
}
