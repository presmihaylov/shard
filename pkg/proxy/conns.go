package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// conns counts the connections each source holds open, over both listeners.
type conns struct {
	limit int

	mu   sync.Mutex
	open map[netip.Addr]int
}

func (c *conns) take(source netip.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[source] >= c.limit {
		return false
	}
	c.open[source]++

	return true
}

func (c *conns) release(source netip.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open[source]--
	if c.open[source] == 0 {
		delete(c.open, source)
	}
}

// capped refuses a connection whose source holds its share, before net/http gives it a goroutine.
type capped struct {
	net.Listener
	server *Server
}

func (s *Server) capped(ln net.Listener) net.Listener {
	return capped{Listener: ln, server: s}
}

func (l capped) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		source := sourceOf(conn.RemoteAddr().String())
		if l.server.conns.take(source) {
			return &counted{Conn: conn, release: sync.OnceFunc(func() { l.server.conns.release(source) })}, nil
		}
		l.server.log.Printf(source, "proxy: refused a connection from %s, which holds %d open, the cap", source, l.server.conns.limit)
		if err := conn.Close(); err != nil {
			return nil, fmt.Errorf("close a connection from %s over the cap: %w", source, err)
		}
	}
}

// counted gives its source's share back on the first Close.
type counted struct {
	net.Conn
	release func()
}

func (c *counted) Close() error {
	c.release()

	return c.Conn.Close()
}

// CloseWrite passes on the half-close net/http sends before it closes, which the embedded net.Conn hides.
func (c *counted) CloseWrite() error {
	cw, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("the connection cannot half-close")
	}

	return cw.CloseWrite()
}
