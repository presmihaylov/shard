package vz

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// A shim that answers its socket owns it; one that does not answer this fast is not there.
const probeTimeout = time.Second

// ErrSocketInUse says a live shim answers on the socket, so a second shim must not replace it.
var ErrSocketInUse = errors.New("vz: a shim already serves this socket")

// Listen claims a shim socket before any VM boots: a live owner is refused, and only a dead one's path is replaced.
func Listen(socket string) (net.Listener, error) {
	conn, err := net.DialTimeout("unix", socket, probeTimeout)
	if err == nil {
		return nil, errors.Join(fmt.Errorf("%w: %s", ErrSocketInUse, socket), conn.Close())
	}
	if !stale(err) {
		return nil, fmt.Errorf("probe the shim socket %s: %w", socket, err)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove the stale shim socket: %w", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen on the shim socket: %w", err)
	}

	return listener, nil
}

// stale is a path with nobody behind it: missing, or bound by a process that is gone.
func stale(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
