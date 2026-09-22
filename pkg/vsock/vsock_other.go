//go:build !linux

package vsock

import "net"

func listen(uint32) (net.Listener, error) { return nil, ErrNotLinux }

func dial(uint32, uint32) (net.Conn, error) { return nil, ErrNotLinux }
