package bundle

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// idShift maps guest ids to host ids in a layer sysbox-runc has chowned into the user namespace; the zero value maps nothing.
type idShift struct {
	uids, gids []specs.LinuxIDMapping
}

// layerShift reads the upper layer's root, because sysbox-runc chowns that layer into the namespace at create and sysbox-mgr chowns it back at delete.
func (b Bundle) layerShift(spec *specs.Spec) (idShift, error) {
	if spec.Linux == nil || len(spec.Linux.UIDMappings) == 0 {
		return idShift{}, nil
	}

	info, err := os.Lstat(b.Upper)
	if err != nil {
		return idShift{}, fmt.Errorf("stat the writable layer %s: %w", b.Upper, err)
	}
	uid, _, err := owner(info)
	if err != nil {
		return idShift{}, err
	}
	if !mapsHost(uid, spec.Linux.UIDMappings) {
		return idShift{}, nil
	}

	return idShift{uids: spec.Linux.UIDMappings, gids: spec.Linux.GIDMappings}, nil
}

// chownIn gives each component of name the host id of its guest owner, since host root's write made it or copied it up unshifted.
func (s idShift) chownIn(root *os.Root, name string) error {
	if len(s.uids) == 0 {
		return nil
	}

	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for i := range parts {
		part := filepath.Join(parts[:i+1]...)

		info, err := root.Lstat(part)
		if err != nil {
			return fmt.Errorf("stat %s in the writable layer: %w", part, err)
		}
		uid, gid, err := owner(info)
		if err != nil {
			return err
		}

		hostUID, hostGID := hostID(uid, s.uids), hostID(gid, s.gids)
		if hostUID == uid && hostGID == gid {
			continue
		}
		if err := root.Lchown(part, int(hostUID), int(hostGID)); err != nil {
			return fmt.Errorf("chown %s in the writable layer to %d:%d: %w", part, hostUID, hostGID, err)
		}
	}

	return nil
}

// hostID is where the guest's id lands, or id itself when it is a host id of the namespace already or the namespace does not map it.
func hostID(id uint32, mappings []specs.LinuxIDMapping) uint32 {
	if mapsHost(id, mappings) {
		return id
	}

	for _, m := range mappings {
		if id >= m.ContainerID && id-m.ContainerID < m.Size {
			return m.HostID + id - m.ContainerID
		}
	}

	return id
}

func mapsHost(id uint32, mappings []specs.LinuxIDMapping) bool {
	return slices.ContainsFunc(mappings, func(m specs.LinuxIDMapping) bool {
		return id >= m.HostID && id-m.HostID < m.Size
	})
}

func owner(info fs.FileInfo) (uint32, uint32, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s reports no owner", info.Name())
	}

	return st.Uid, st.Gid, nil
}
