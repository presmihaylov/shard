package sysbox

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
)

// Capabilities: no checkpoint verb, and a port forward, which dials from the host into the network namespace the daemon made.
func (p *Provider) Capabilities() models.Capabilities { return models.Capabilities{Port: true} }

// DialPort opens one connection to port on the sandbox's loopback, from a host thread inside its network namespace.
func (p *Provider) DialPort(ctx context.Context, id string, port uint16) (net.Conn, error) {
	path, err := p.netnsOf(ctx, id)
	if err != nil {
		return nil, err
	}
	conn, err := netns.DialIn(ctx, path, port)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return conn, nil
}

// netnsOf answers the namespace config.json names, or, with none named, the one the runtime made for the sandbox's init.
func (p *Provider) netnsOf(ctx context.Context, id string) (string, error) {
	b, err := p.open(id)
	if err != nil {
		return "", err
	}
	runtime, err := b.Runtime()
	if err != nil {
		return "", fmt.Errorf("sandbox %s: %w", id, err)
	}
	if runtime.NetnsPath != "" {
		return runtime.NetnsPath, nil
	}
	state, err := p.runner.State(ctx, id)
	if err != nil {
		return "", fmt.Errorf("sandbox %s: %w", id, err)
	}
	if state.PID == 0 {
		return "", fmt.Errorf("sandbox %s runs no init whose network namespace a port could be dialed in", id)
	}

	return filepath.Join(p.procRoot, strconv.Itoa(state.PID), "ns", "net"), nil
}
