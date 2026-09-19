//go:build !linux

package vsock

import "net"

func listen(uint32) (net.Listener, error) { return nil, ErrNotLinux }
