package vzvm

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// DialPort opens one connection to port on the guest's loopback, over its own vsock stream to shard-init, admitted as an exec is.
func (p *Provider) DialPort(ctx context.Context, id string, port uint16) (net.Conn, error) {
	if verb := p.holding(id); verb != "" {
		return nil, fmt.Errorf("sandbox %s: a %s holds the sandbox frozen, so no connection reaches it until that ends", id, verb)
	}
	m, _, err := p.running(ctx, id)
	if err != nil {
		return nil, err
	}
	if !m.ports.Load() {
		return nil, fmt.Errorf("sandbox %s runs a shard-init from before port forwards: stop and start it to forward a port: %w", id, models.Unsupported(Name, models.VerbPort))
	}

	conn, err := p.admitForward(ctx, m, id)
	if err != nil {
		return nil, err
	}
	forward, err := supervisor.OpenForward(ctx, conn, port)
	if err != nil {
		m.closeExec(conn)

		return nil, errors.Join(fmt.Errorf("sandbox %s: %w", id, err), conn.Close())
	}

	return &forwarded{Forward: forward, m: m, conn: conn}, nil
}

// admitForward dials the forward port under the gate an exec's dial takes, and tracks the stream so a save that resets it ends it.
func (p *Provider) admitForward(ctx context.Context, m *machine, id string) (net.Conn, error) {
	m.admit.RLock()
	defer m.admit.RUnlock()
	if verb := m.holder.Load(); verb != nil {
		return nil, fmt.Errorf("sandbox %s: a %s holds the sandbox frozen, so no connection reaches it until that ends", id, *verb)
	}
	conn, err := m.dial(ctx, supervisor.ForwardPort)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: open a forward stream: %w", id, err)
	}
	m.openExec(conn)

	return conn, nil
}

// forwarded lets the machine stop tracking its stream once the forward closes.
type forwarded struct {
	*supervisor.Forward
	m    *machine
	conn net.Conn
}

func (f *forwarded) Close() error {
	f.m.closeExec(f.conn)

	return f.Forward.Close()
}
