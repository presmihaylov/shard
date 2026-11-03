package linkfwd

import "net"

// conn is a flow's target end; closing it forgets the flow.
type conn struct {
	net.Conn
	f  *Forwarder
	fl *flow
}

func (c *conn) Close() error {
	c.f.untrack(c.fl)

	return c.Conn.Close()
}

// halfConn passes a half close through to a target end that takes one, which a splice looks for.
type halfConn struct {
	*conn
	w interface{ CloseWrite() error }
}

func (c halfConn) CloseWrite() error { return c.w.CloseWrite() }

func wrap(f *Forwarder, fl *flow, target net.Conn) net.Conn {
	c := &conn{Conn: target, f: f, fl: fl}
	if w, ok := target.(interface{ CloseWrite() error }); ok {
		return halfConn{conn: c, w: w}
	}

	return c
}
