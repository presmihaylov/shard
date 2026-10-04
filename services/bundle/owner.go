package bundle

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/presmihaylov/shard/pkg/netns"
)

// idShift is the user namespace sysbox-runc has chowned a layer into; the zero value maps nothing.
type idShift netns.IDMapping

// layerShift reads the upper layer's root, because sysbox-runc chowns that layer into the namespace at create and sysbox-mgr chowns it back at delete.
func (b Bundle) layerShift() (idShift, error) {
	if !b.Userns.Set() {
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
	shift := idShift(b.Userns)
	if !shift.mapsHost(uid) {
		return idShift{}, nil
	}

	return shift, nil
}

// chownIn gives each component of name the host id of its guest owner, since host root's write made it or copied it up unshifted.
func (s idShift) chownIn(root *os.Root, name string) error {
	if s.Size == 0 {
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

		hostUID, hostGID := s.hostID(uid), s.hostID(gid)
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
func (s idShift) hostID(id uint32) uint32 {
	if s.mapsHost(id) || id >= s.Size {
		return id
	}

	return s.HostID + id
}

func (s idShift) mapsHost(id uint32) bool {
	return id >= s.HostID && id-s.HostID < s.Size
}

func owner(info fs.FileInfo) (uint32, uint32, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s reports no owner", info.Name())
	}

	return st.Uid, st.Gid, nil
}
