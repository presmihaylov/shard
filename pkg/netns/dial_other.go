//go:build !linux

package netns

import (
	"context"
	"fmt"
	"net"
)

// DialIn has no network namespace to enter off Linux.
func DialIn(_ context.Context, path string, port uint16) (net.Conn, error) {
	return nil, fmt.Errorf("dial port %d in %s: %w", port, path, ErrNotLinux)
}
