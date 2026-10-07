package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

// ForwardHeader opens a forward connection: the port on the guest's own loopback that shard-init dials for the host.
type ForwardHeader struct {
	Port uint16 `json:"port"`
}

// ForwardReply is the guest's one line before the bytes; an empty Error means the dial took and the bytes follow.
type ForwardReply struct {
	Error string `json:"error,omitempty"`
	// Refused says nothing listens on the port, which the host names apart from a guest that broke.
	Refused bool `json:"refused,omitempty"`
}

// OpenForward asks the guest behind conn, a fresh forward port connection, for port, and answers the connection that carries its bytes.
func OpenForward(ctx context.Context, conn net.Conn, port uint16) (*Forward, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("forward port %d: %w", port, err)
		}
	}
	if err := WriteMessage(conn, ForwardHeader{Port: port}); err != nil {
		return nil, fmt.Errorf("forward port %d: %w", port, err)
	}
	var reply ForwardReply
	if err := ReadHeader(conn, &reply); err != nil {
		return nil, fmt.Errorf("forward port %d: %w", port, err)
	}
	if reply.Refused {
		return nil, fmt.Errorf("forward port %d: nothing listens on it in the guest: %w", port, syscall.ECONNREFUSED)
	}
	if reply.Error != "" {
		return nil, fmt.Errorf("forward port %d: the guest cannot reach it: %s", port, reply.Error)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("forward port %d: %w", port, err)
	}

	return &Forward{Conn: conn}, nil
}

// Forward is the host end of a forward: its bytes go as frames, so a half close reaches the guest as one.
type Forward struct {
	net.Conn
}

func (c *Forward) Write(p []byte) (int, error) {
	if err := WriteFrame(c.Conn, StreamStdin, p); err != nil {
		return 0, err
	}

	return len(p), nil
}

// CloseWrite says the host sends no more; neither vsock proxy passes a half close of the stream itself.
func (c *Forward) CloseWrite() error {
	return WriteFrame(c.Conn, StreamStdinClose, nil)
}

// ForwardGuest is the guest end of a forward: it reads the host's frames as plain bytes and its half close as io.EOF.
type ForwardGuest struct {
	net.Conn
	frames  *bufio.Reader
	pending []byte
	closed  bool
}

// NewForwardGuest wraps conn once the header is read off it and the reply written.
func NewForwardGuest(conn net.Conn) *ForwardGuest {
	return &ForwardGuest{Conn: conn, frames: bufio.NewReader(conn)}
}

func (c *ForwardGuest) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		if c.closed {
			return 0, io.EOF
		}
		stream, payload, err := ReadFrame(c.frames)
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("the host dropped the forward: %w", syscall.ECONNRESET)
		}
		if err != nil {
			return 0, err
		}
		switch stream {
		case StreamStdin:
			c.pending = payload
		case StreamStdinClose:
			c.closed = true
		default:
			return 0, fmt.Errorf("a forward carries no stream %d", stream)
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]

	return n, nil
}

// CloseWrite passes the guest's half close to the host, when the stream underneath takes one.
func (c *ForwardGuest) CloseWrite() error {
	closer, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return c.Close()
	}

	return closer.CloseWrite()
}
