//go:build !linux && !darwin

package firecracker

import (
	"errors"
	"net"
)

func peerPID(net.Conn) (int, error) {
	return 0, errors.New("firecracker: the peer of a unix socket is read on linux and darwin only")
}
