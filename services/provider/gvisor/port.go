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

	return forwardStream{conn}, nil
}

// forwardStream takes a half close and passes none on: the sentry ends the whole connection at its stream's EOF, before the answer is back.
type forwardStream struct {
	net.Conn
}

func (forwardStream) CloseWrite() error { return nil }
