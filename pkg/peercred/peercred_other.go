//go:build !linux && !darwin

package peercred

import (
	"errors"
	"net"
)

// PID is the process behind a unix socket connection, which only linux and darwin attest.
func PID(net.Conn) (int, error) {
	return 0, errors.New("peercred: the peer of a unix socket is read on linux and darwin only")
}
