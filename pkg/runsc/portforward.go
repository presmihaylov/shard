package runsc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// PortForwardSocket is the socket a port forward listens on, under a scratch directory of the exec dir, for the daemon's path bound.
const PortForwardSocket = "pf-0123456789/s"

// PortForward opens one stream to port on the sandbox's own loopback: runsc dials the socket it is given and hands that end into the sandbox, then exits.
func (r *Runner) PortForward(ctx context.Context, id string, port uint16) (conn net.Conn, err error) {
	dir, err := os.MkdirTemp(r.execDir, "pf-")
	if err != nil {
		return nil, fmt.Errorf("create a directory for the port forward socket: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listen for runsc port-forward: %w", err)
	}
	type accepted struct {
		conn net.Conn
		err  error
	}
	got := make(chan accepted, 1)
	go func() {
		conn, err := l.Accept()
		got <- accepted{conn, err}
	}()

	if err := r.run(ctx, nil, "port-forward", "--stream", socket, id, strconv.Itoa(int(port))); err != nil {
		// The close ends an accept runsc never dialed, so a failed run never leaves it waiting.
		closed := l.Close()
		res := <-got

		return nil, errors.Join(err, quietClose(closed), closeIfAny(res.conn))
	}

	// runsc dialed before it exited, so the stream is queued even where the accept has not taken it yet; a close now would drop it.
	var res accepted
	select {
	case res = <-got:
	case <-ctx.Done():
		closed := l.Close()
		res = <-got

		return nil, errors.Join(fmt.Errorf("runsc port-forward %s %d: %w", id, port, ctx.Err()), quietClose(closed), closeIfAny(res.conn))
	}
	if res.err != nil {
		return nil, errors.Join(fmt.Errorf("runsc port-forward %s %d: accept its stream: %w", id, port, res.err), quietClose(l.Close()))
	}
	if err := l.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("close the port forward socket: %w", err), res.conn.Close())
	}

	return res.conn, nil
}

func closeIfAny(conn net.Conn) error {
	if conn == nil {
		return nil
	}

	return conn.Close()
}

func quietClose(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}
