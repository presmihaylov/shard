package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// PlainPort and TLSPort are where the proxy listens on the gateway address: the host sends a fronted sandbox's 80 to the first and its 443 to the second.
	PlainPort = 30080
	TLSPort   = 30443

	// BodyCap bounds the body the proxy holds to rewrite; a longer one streams through unchanged.
	BodyCap = 8 << 20

	readHeaderTimeout = 30 * time.Second
	shutdownGrace     = 5 * time.Second
)

// Request is what the proxy knows about one request before it asks the director.
type Request struct {
	// Source is the address the connection came from, which is the sandbox's own.
	Source netip.Addr
	// Host is the name the request is bound for, lowercase and without a trailing dot.
	Host string
	// Port is the port the guest dialed: 80 on the plain listener, 443 on the TLS one.
	Port int
	TLS  bool
}

// Decision is the director's verdict, and where to dial when it allows.
type Decision struct {
	Allowed  bool
	Upstream netip.AddrPort
	// Rule and Reason name what decided, and go into the 403 body.
	Rule   string
	Reason string
}

// Director judges every request and rewrites the allowed ones; the proxy itself knows no policy and no secret.
type Director interface {
	Decide(ctx context.Context, req Request) (Decision, error)
	// Rewrite edits the outbound request in place; body is nil when it was too long to hold, and what comes back is sent.
	Rewrite(ctx context.Context, req Request, out *http.Request, body []byte) ([]byte, error)
}

// Config is what a Server is built from.
type Config struct {
	// Address is the gateway address both listeners bind, so only the bridge reaches them.
	Address  netip.Addr
	CA       *CA
	Director Director
	Log      *log.Logger
}

// Server terminates plain HTTP and TLS from fronted sandboxes and forwards what the director allows.
type Server struct {
	cfg       Config
	transport *http.Transport
}

func New(cfg Config) (*Server, error) {
	if cfg.CA == nil || cfg.Director == nil || cfg.Log == nil {
		return nil, errors.New("the proxy needs a CA, a director and a log")
	}
	if !cfg.Address.IsValid() {
		return nil, errors.New("the proxy needs an address to listen on")
	}

	var dialer net.Dialer

	return &Server{
		cfg: cfg,
		transport: &http.Transport{
			// The director resolved the name once and judged that address, so that address is what is dialed.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				upstream, ok := ctx.Value(upstreamKey{}).(netip.AddrPort)
				if !ok {
					return nil, errors.New("no upstream was pinned for this request")
				}

				return dialer.DialContext(ctx, "tcp", upstream.String())
			},
			// A pooled connection would outlive the decision that opened it, so every request dials afresh.
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}, nil
}

type upstreamKey struct{}

// Run listens on both ports at the address and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	plain, err := net.Listen("tcp", netip.AddrPortFrom(s.cfg.Address, PlainPort).String())
	if err != nil {
		return fmt.Errorf("listen for plain http: %w", err)
	}

	secure, err := net.Listen("tcp", netip.AddrPortFrom(s.cfg.Address, TLSPort).String())
	if err != nil {
		return errors.Join(fmt.Errorf("listen for tls: %w", err), plain.Close())
	}

	return s.Serve(ctx, plain, secure)
}

// Serve runs the proxy over two listeners it then owns, so a test can hand it loopback ports.
func (s *Server) Serve(ctx context.Context, plain, secure net.Listener) error {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Without a name there is nothing to judge, so the handshake is refused.
			if hello.ServerName == "" {
				return nil, errors.New("the proxy needs the server name in the tls handshake")
			}

			return s.cfg.CA.Leaf(canonicalHost(hello.ServerName))
		},
	}

	servers := []*http.Server{
		{Handler: s.handler(false), ReadHeaderTimeout: readHeaderTimeout, ErrorLog: s.cfg.Log},
		{Handler: s.handler(true), ReadHeaderTimeout: readHeaderTimeout, ErrorLog: s.cfg.Log},
	}
	listeners := []net.Listener{plain, tls.NewListener(secure, tlsConfig)}

	errs := make(chan error, len(servers))

	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Go(func() {
			if err := srv.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		})
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errs:
	}

	// Shutdown drains what is in flight, then Close cuts what is still open, so a stop never waits on a guest.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	for _, srv := range servers {
		if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
			err = errors.Join(err, srv.Close())
		}
	}
	wg.Wait()

	return err
}

func (s *Server) handler(secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handle(w, r, secure)
	})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, secure bool) {
	req, err := request(r, secure)
	if err != nil {
		s.refuse(w, r, http.StatusBadRequest, err.Error())

		return
	}

	decision, err := s.cfg.Director.Decide(r.Context(), req)
	if err != nil {
		s.cfg.Log.Printf("proxy: %s %s %s:%d: %v", req.Source, r.Method, req.Host, req.Port, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})

		return
	}
	if !decision.Allowed {
		s.cfg.Log.Printf("proxy: %s %s %s:%d denied by %s", req.Source, r.Method, req.Host, req.Port, decision.Rule)
		deny(w, req, decision)

		return
	}

	out, err := s.outbound(r, req, decision)
	if err != nil {
		s.cfg.Log.Printf("proxy: %s %s %s:%d: %v", req.Source, r.Method, req.Host, req.Port, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})

		return
	}

	sw := &statusWriter{ResponseWriter: w}
	s.forward().ServeHTTP(sw, out) //nolint:gosec // forwarding the guest's request is the job, and the director pinned where it dials
	s.cfg.Log.Printf("proxy: %s %s %s:%d %d", req.Source, r.Method, req.Host, req.Port, sw.status)
}

// request reads who is asking and for what; the name must be one the host can judge.
func request(r *http.Request, secure bool) (Request, error) {
	source, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return Request{}, fmt.Errorf("the connection came from %q, which is not an address", r.RemoteAddr)
	}

	host := canonicalHost(hostOnly(r.Host))
	if host == "" {
		return Request{}, errors.New("the request names no host")
	}

	req := Request{Source: source.Addr().Unmap(), Host: host, Port: 80, TLS: secure}
	if !secure {
		return req, nil
	}

	// The certificate was minted for the handshake name, so a Host header naming another lies to one of them.
	if r.TLS == nil || canonicalHost(r.TLS.ServerName) != host {
		return Request{}, fmt.Errorf("the host header names %s and the tls handshake named another", host)
	}
	req.Port = 443

	return req, nil
}

// outbound builds the request the upstream sees: the guest's, with the director's edits and the body it may hold.
func (s *Server) outbound(r *http.Request, req Request, decision Decision) (*http.Request, error) {
	held, rest, err := readBody(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read the request body: %w", err)
	}

	out := r.Clone(context.WithValue(r.Context(), upstreamKey{}, decision.Upstream))
	out.RequestURI = ""
	out.URL.Scheme = "http"
	if req.TLS {
		out.URL.Scheme = "https"
	}
	out.URL.Host = req.Host

	body, err := s.cfg.Director.Rewrite(out.Context(), req, out, held)
	if err != nil {
		return nil, err
	}

	if rest != nil {
		out.Body = rest

		return out, nil
	}
	if held == nil {
		return out, nil
	}

	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return out, nil
}

// readBody holds up to BodyCap; past that it hands back a reader over what was read and what is still coming.
func readBody(body io.ReadCloser) ([]byte, io.ReadCloser, error) {
	if body == nil || body == http.NoBody {
		return nil, nil, nil
	}

	held, err := io.ReadAll(io.LimitReader(body, BodyCap+1))
	if err != nil {
		return nil, nil, err
	}
	if len(held) <= BodyCap {
		return held, nil, nil
	}

	return nil, &joinedBody{Reader: io.MultiReader(bytes.NewReader(held), body), closer: body}, nil
}

type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (j *joinedBody) Close() error { return j.closer.Close() }

func (s *Server) forward() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// The outbound request is already built, so the rewrite has nothing left to do.
		Rewrite:   func(*httputil.ProxyRequest) {},
		Transport: s.transport,
		ErrorLog:  s.cfg.Log,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream " + r.URL.Host + ": " + err.Error()})
		},
	}
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.cfg.Log.Printf("proxy: %s %s: %d %s", r.RemoteAddr, r.Method, status, message)
	writeJSON(w, status, map[string]string{"error": message})
}

func deny(w http.ResponseWriter, req Request, decision Decision) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error":  "denied by the egress policy",
		"host":   req.Host,
		"port":   strconv.Itoa(req.Port),
		"rule":   decision.Rule,
		"reason": decision.Reason,
	})
}

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode the error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A client that hung up before its error landed is the only failure here, and there is no one left to tell.
	_, _ = w.Write(append(encoded, '\n')) //nolint:errcheck
}

// statusWriter records the status for the one log line, and unwraps so the reverse proxy can still flush and hijack.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func hostOnly(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}

	return host
}

func canonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}
