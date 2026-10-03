package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// A keep-alive connection that sends no next request is closed after the idle timeout, where net/http's default waits for ever (SHARD-350).
func TestProxyClosesAKeepAliveConnectionThatGoesIdle(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(echoHandler), func(s *Server) { s.idle = 200 * time.Millisecond })

	conn, err := net.Dial("tcp", h.plain.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	answers := bufio.NewReader(conn)
	if err := ask(conn, answers); err != nil {
		t.Fatalf("the first request: %v", err)
	}

	start := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := answers.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection read %v, want the proxy's close", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("the proxy held an idle connection %s", waited)
	}
}

// One source holds at most its share over both ports: the next connection is closed and named in the log, and one that ends gives its place back (SHARD-350).
func TestProxyRefusesASourcePastItsConnectionCap(t *testing.T) {
	var server *Server
	h := newHarness(t, http.HandlerFunc(echoHandler), func(s *Server) {
		s.conns.limit = 2
		server = s
	})

	plain := dialAndAsk(t, h, false)
	secure := dialAndAsk(t, h, true)
	defer secure.Close()

	refused, err := net.Dial("tcp", h.plain.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if err := refused.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ask(refused, bufio.NewReader(refused)); err == nil {
		t.Fatal("a connection past the cap got an answer")
	}
	awaitAccounted(t, h, "refused a connection from 127.0.0.1, which holds 2 open, the cap", 1)

	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for open(server, netip.MustParseAddr("127.0.0.1")) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("a closed connection never gave its place back")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dialAndAsk(t, h, true).Close()
}

// dialAndAsk opens a connection to one of the proxy's ports and gets one answer on it, so the proxy has counted it.
func dialAndAsk(t *testing.T, h *harness, secure bool) net.Conn {
	t.Helper()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.ca.CertPEM())
	dial := func() (net.Conn, error) { return net.Dial("tcp", h.plain.Addr().String()) }
	if secure {
		dial = func() (net.Conn, error) {
			return tls.Dial("tcp", h.secure.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "api.test", MinVersion: tls.VersionTLS12})
		}
	}
	conn, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	if err := ask(conn, bufio.NewReader(conn)); err != nil {
		t.Fatalf("the request on a connection under the cap: %v", err)
	}

	return conn
}

// ask sends one keep-alive request for api.test and reads its whole answer, whatever the status.
func ask(conn net.Conn, answers *bufio.Reader) error {
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: api.test\r\n\r\n"); err != nil {
		return err
	}
	resp, err := http.ReadResponse(answers, nil)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return errors.Join(err, resp.Body.Close())
	}

	return resp.Body.Close()
}

func open(s *Server, source netip.Addr) int {
	s.conns.mu.Lock()
	defer s.conns.mu.Unlock()

	return s.conns.open[source]
}
