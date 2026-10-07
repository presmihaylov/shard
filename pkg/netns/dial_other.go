//go:build !linux

package netns

import (
	"context"
	"net"
)

// DialIn has no network namespace to enter off Linux.
func DialIn(context.Context, string, uint16) (net.Conn, error) { return nil, ErrNotLinux }
