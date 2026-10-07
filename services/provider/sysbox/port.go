package sysbox

import (
	"context"
	"fmt"
	"net"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
)

// Capabilities: no checkpoint verb, and a port forward, which dials from the host into the network namespace the daemon made.
func (p *Provider) Capabilities() models.Capabilities { return models.Capabilities{Port: true} }

// DialPort opens one connection to port on the sandbox's loopback, from a host thread inside the namespace config.json names.
func (p *Provider) DialPort(ctx context.Context, id string, port uint16) (net.Conn, error) {
	b, err := p.open(id)
	if err != nil {
		return nil, err
	}
	runtime, err := b.Runtime()
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	if runtime.NetnsPath == "" {
		return nil, fmt.Errorf("sandbox %s has no network namespace on the host to dial port %d in", id, port)
	}
	conn, err := netns.DialIn(ctx, runtime.NetnsPath, port)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return conn, nil
}
