package gvisor

import (
	"context"
	"fmt"
	"net"
)

// DialPort opens one connection to port on the sandbox's own loopback, which runsc hands into the sentry's netstack.
func (p *Provider) DialPort(ctx context.Context, id string, port uint16) (net.Conn, error) {
	conn, err := p.runsc.PortForward(ctx, id, port)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return conn, nil
}
