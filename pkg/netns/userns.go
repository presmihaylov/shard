package netns

import (
	"context"
	"fmt"
	"path/filepath"
)

// UsernsRunDir is where a user namespace this manager made is pinned, beside iproute2's netns pins.
const UsernsRunDir = "/var/run/shard/userns"

// IDMapping is where guest id 0 lands on the host and how many ids follow it. The guest maps 0..Size
// onto HostID..HostID+Size, for uids and gids alike.
type IDMapping struct {
	HostID uint32
	Size   uint32
}

// Set reports whether a mapping asks for a user namespace at all.
func (m IDMapping) Set() bool { return m.Size != 0 }

// UsernsPath is what an OCI spec points at to join a user namespace this manager made.
func UsernsPath(name string) string {
	return filepath.Join(UsernsRunDir, name)
}

// AddOwnedNamespace creates a named network namespace owned by a new user namespace with the given
// mapping, and pins both. A guest that joins the pair has CAP_NET_ADMIN over its own netns, which a
// namespace made in the host's user namespace never grants it. Both survive until DeleteNamespace.
func (m *Manager) AddOwnedNamespace(ctx context.Context, name string, owner IDMapping) error {
	if !owner.Set() {
		return fmt.Errorf("namespace %s: an owned namespace needs a mapping with a size", name)
	}

	return m.addOwnedNamespace(ctx, name, owner)
}
