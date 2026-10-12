package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
)

// conns counts what each source holds open over both listeners: its connections, or its requests.
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
			c := &counted{Conn: conn, source: source}
			c.release = sync.OnceFunc(func() {
				l.server.conns.release(source)
				if c.multiplexed.Load() {
					l.server.h2conns.release(source)
				}
			})

			return c, nil
		}
		l.server.log.Printf(source, "proxy: refused a connection from %s, which holds %d open, the cap", source, l.server.conns.limit)
		if err := conn.Close(); err != nil {
			return nil, fmt.Errorf("close a connection from %s over the cap: %w", source, err)
		}
	}
}

// counted gives its source's shares back on the first Close.
type counted struct {
	net.Conn
	source      netip.Addr
	multiplexed atomic.Bool
	release     func()
}

// multiplex offers h2 to a client that asks for it while its source holds an h2 share left, which the connection then holds.
func (s *Server) multiplex(hello *tls.ClientHelloInfo) bool {
	c, ok := hello.Conn.(*counted)
	if !ok || !slices.Contains(hello.SupportedProtos, "h2") || !s.h2conns.take(c.source) {
		return false
	}
	// Only Serve's own stop closes a connection mid-handshake, and then no share matters.
	c.multiplexed.Store(true)

	return true
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
