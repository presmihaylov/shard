package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"
)

const (
	// PlainPort and TLSPort are what the proxy stands in for: HTTP on one, HTTPS on the other.
	PlainPort = 80
	TLSPort   = 443

	headerTimeout = 30 * time.Second
	// A request to a granted host is held in memory while its body arrives, so a slow one cannot hold it for long.
	requestTimeout   = 5 * time.Minute
	handshakeTimeout = 10 * time.Second
)

// Route is where one request goes. Target is the address the Director checked, so the proxy dials
// that and never resolves the name again. Rewrite changes the request on its way out, and nil leaves it.
type Route struct {
	Target  netip.AddrPort
	Rewrite func(*http.Request) error
}

// Director decides every request: the sandbox it came from, by source address, the host it names and
// the port it was bound for. It returns *Denied for one that may not go.
type Director interface {
	Route(ctx context.Context, source netip.Addr, host string, port int) (Route, error)
}

// Denied is a Director's refusal. The proxy answers it with 403 and the reason, so the guest learns
// why and nothing else.
type Denied struct {
	Reason string
}

func (d *Denied) Error() string { return d.Reason }

// Option shapes a Server.
type Option func(*Server)

// WithRootCAs sets what the proxy verifies an upstream against, in place of the system roots.
func WithRootCAs(pool *x509.CertPool) Option {
	return func(s *Server) { s.rootCAs = pool }
}

// Server is one proxy: a CA to speak for every host, and a Director to say where a request may go.
type Server struct {
	ca       *CA
	director Director
	rootCAs  *x509.CertPool
}

// New builds a Server. It listens on nothing until Serve is called.
func New(ca *CA, director Director, opts ...Option) *Server {
	s := &Server{ca: ca, director: director}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// ServePlain carries HTTP that was bound for port 80. It returns when the listener closes.
func (s *Server) ServePlain(l net.Listener) error {
	srv := &http.Server{Handler: s.handler(PlainPort, "http"), ReadHeaderTimeout: headerTimeout, ReadTimeout: requestTimeout}

	return srv.Serve(l)
}

// ServeTLS terminates HTTPS bound for port 443; a client that names no host gets no certificate, since the proxy cannot tell where it was going.
func (s *Server) ServeTLS(l net.Listener) error {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "" {
				return nil, errors.New("the client named no host, so the proxy cannot say which one it is")
			}

			return s.ca.Leaf(hello.ServerName)
		},
	}

	srv := &http.Server{Handler: s.handler(TLSPort, "https"), ReadHeaderTimeout: headerTimeout, ReadTimeout: requestTimeout}

	return srv.Serve(tls.NewListener(l, cfg))
}

type targetKey struct{}

func (s *Server) handler(port int, scheme string) http.Handler {
	upstream := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = pr.In.Host
			pr.Out.Host = pr.In.Host
		},
		Transport: s.transport(),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("shard: reach %s: %v", r.Host, err), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source, err := sourceOf(r.RemoteAddr)
		if err != nil {
			http.Error(w, "shard: "+err.Error(), http.StatusBadRequest)

			return
		}

		host, err := hostOf(r)
		if err != nil {
			http.Error(w, "shard: "+err.Error(), http.StatusBadRequest)

			return
		}

		route, err := s.director.Route(r.Context(), source, host, port)
		var denied *Denied
		if errors.As(err, &denied) {
			http.Error(w, "shard: "+denied.Reason, http.StatusForbidden)

			return
		}
		if err != nil {
			http.Error(w, "shard: route "+host+": "+err.Error(), http.StatusBadGateway)

			return
		}

		if route.Rewrite != nil {
			if err := route.Rewrite(r); err != nil {
				http.Error(w, "shard: rewrite the request to "+host+": "+err.Error(), http.StatusBadGateway)

				return
			}
		}

		// The target is what the Director checked, so the dial goes where the policy said.
		upstream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, route.Target))) //nolint:gosec // G704: the Director pinned it
	})
}

// transport dials the target the Director pinned, whatever name the request carries, and verifies
// an upstream certificate against that name. HTTP/2 stays off: the guest side speaks HTTP/1.1.
func (s *Server) transport() *http.Transport {
	dialer := &net.Dialer{Timeout: handshakeTimeout}

	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			target, ok := ctx.Value(targetKey{}).(netip.AddrPort)
			if !ok {
				return nil, errors.New("the request carries no target")
			}

			return dialer.DialContext(ctx, "tcp", target.String())
		},
		TLSClientConfig:     &tls.Config{RootCAs: s.rootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: handshakeTimeout,
		// A fresh connection per request keeps a rotated value from riding a connection made under the old one.
		DisableKeepAlives: true,
	}
}

func sourceOf(remote string) (netip.Addr, error) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("the connection has no source address: %w", err)
	}

	return ap.Addr().Unmap(), nil
}

// hostOf is the name the request is for. Over TLS the connection already named one, and a request
// that names another is refused rather than sent to the first under the second's name.
func hostOf(r *http.Request) (string, error) {
	named := strings.ToLower(strings.TrimSuffix(stripPort(r.Host), "."))
	if r.TLS == nil {
		if named == "" {
			return "", errors.New("the request names no host")
		}

		return named, nil
	}

	sni := strings.ToLower(strings.TrimSuffix(r.TLS.ServerName, "."))
	if named != "" && named != sni {
		return "", fmt.Errorf("the request names %s but the connection was made for %s", named, sni)
	}

	return sni, nil
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}

	return host
}
