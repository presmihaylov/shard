// Package vsock is the guest end of a virtio-vsock stream: a listener on one guest port, socket calls only.
package vsock

import (
	"errors"
	"fmt"
	"net"
)

// ErrNotLinux keeps a developer Mac honest: only a Linux guest has a vsock device to listen on.
var ErrNotLinux = errors.New("a vsock listener needs Linux")

// Addr names one vsock endpoint. Every guest of a VM is context id 3, so the port is the whole name.
type Addr struct {
	Port uint32
}

func (a Addr) Network() string { return "vsock" }
func (a Addr) String() string  { return fmt.Sprintf("vsock:%d", a.Port) }

// Listen binds one guest port and accepts the host's connections to it.
func Listen(port uint32) (net.Listener, error) { return listen(port) }
