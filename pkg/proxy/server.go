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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/shard/pkg/lograte"
)

const (
	// PlainPort and TLSPort are where the proxy listens on the gateway address: the host sends a fronted sandbox's 80 to the first and its 443 to the second.
	PlainPort = 30080
	TLSPort   = 30443

	// BodyCap bounds the body the proxy holds to rewrite; a longer one streams through unchanged.
	BodyCap = 8 << 20
	// heldChunk is the first buffer of a held body that names no length.
	heldChunk = 32 << 10

	readHeaderTimeout = 30 * time.Second
	// idleTimeout ends a keep-alive connection that sends no next request, where net/http's default waits for ever (SHARD-350).
	idleTimeout = 60 * time.Second
	// MaxSourceConns bounds the connections one source holds open over both ports, the share the host gives each sandbox too (SHARD-350).
	MaxSourceConns = 1024
	// maxHeaderBytes bounds one request's headers, where net/http's default of 1 MiB let a guest write a megabyte of log per request (SHARD-345).
	maxHeaderBytes = 64 << 10
	// maxHostLen is the longest DNS name, and so the longest host the proxy judges or prints.
	maxHostLen = 253
)

var (
	// clientGoneGrace is how long a request goes on after its client hung up; none goes on for ever.
	clientGoneGrace = 30 * time.Second
	shutdownGrace   = 5 * time.Second
	// heldReadTimeout bounds the read of a held body alone; a server ReadTimeout would also cut a long upload that streams.
	heldReadTimeout = 30 * time.Second
	// heldBudget bounds what one source holds at once to put secrets in, its bodies and what a value adds, so it cannot fill the daemon (SHARD-348).
	heldBudget = 32 << 20
)

var (
	errStalled    = errors.New("the request body stalled")
	errOverBudget = errors.New("the requests of this sandbox hold the most the proxy keeps to put secrets in")
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
	// Hold says the director could put a secret in the body, so the proxy reads it first; every other body streams (SHARD-348).
	Hold bool
	// Rule and Reason name what decided, and go into the 403 body.
	Rule   string
	Reason string
}

// Reserve charges n more bytes to the source of the request, and fails past its budget.
type Reserve func(n int) error

// Director judges every request and rewrites the allowed ones; the proxy itself knows no policy and no secret.
type Director interface {
	Decide(ctx context.Context, req Request) (Decision, error)
	// Rewrite edits the outbound request in place and reserves each byte a value adds before it exists; body is nil when none was held, and what comes back is sent.
	Rewrite(ctx context.Context, req Request, out *http.Request, body []byte, reserve Reserve) ([]byte, error)
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
	cfg Config
	// log bounds each source's fault and deny lines; the line of an answered request is written whole (SHARD-347).
	log       *lograte.Log
	transport *http.Transport
	goneGrace time.Duration
	held      budget
	idle      time.Duration
	conns     *conns
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
		cfg:       cfg,
		log:       lograte.New(cfg.Log, "proxy"),
		goneGrace: clientGoneGrace,
		held:      budget{limit: heldBudget, by: map[netip.Addr]int{}},
		idle:      idleTimeout,
		conns:     &conns{limit: MaxSourceConns, open: map[netip.Addr]int{}},
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
			if len(hello.ServerName) > maxHostLen {
				return nil, errors.New("the tls server name is longer than a dns name")
			}

			return s.cfg.CA.Leaf(canonicalHost(hello.ServerName))
		},
	}

	stopped, stop := context.WithCancel(context.Background())
	// net/http logs a failed tls handshake per connection, naming the guest after "from".
	errorLog := s.log.Logger(namedSource)
	servers := []*http.Server{
		{Handler: s.handler(stopped, false), ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: s.idle, MaxHeaderBytes: maxHeaderBytes, ErrorLog: errorLog},
		{Handler: s.handler(stopped, true), ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: s.idle, MaxHeaderBytes: maxHeaderBytes, ErrorLog: errorLog},
	}
	listeners := []net.Listener{s.capped(plain), tls.NewListener(s.capped(secure), tlsConfig)}

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
	// Close leaves its handlers running, and a request whose client left no longer ends with the connection.
	stop()
	wg.Wait()

	return err
}

func (s *Server) handler(stopped context.Context, secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handle(stopped, w, r, secure)
	})
}

func (s *Server) handle(stopped context.Context, w http.ResponseWriter, r *http.Request, secure bool) {
	req, err := request(r, secure)
	if err != nil {
		s.refuse(w, r, http.StatusBadRequest, err.Error())

		return
	}

	// net/http cancels r.Context() on a half-close, which a fire-and-forget client sends right after its request (SHARD-238).
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	afterGone := context.AfterFunc(r.Context(), func() {
		grace := time.AfterFunc(s.goneGrace, cancel)
		context.AfterFunc(ctx, func() { grace.Stop() })
	})
	defer afterGone()
	// The grace is the client's, never the server's: a stopped proxy cuts the request at once.
	afterStop := context.AfterFunc(stopped, cancel)
	defer afterStop()

	decision, err := s.cfg.Director.Decide(ctx, req)
	if err != nil {
		s.log.Printf(req.Source, "proxy: %s %s %s:%d: %v", req.Source, clip(r.Method), req.Host, req.Port, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})

		return
	}
	if !decision.Allowed {
		s.log.Printf(req.Source, "proxy: %s %s %s:%d denied by %s", req.Source, clip(r.Method), req.Host, req.Port, decision.Rule)
		deny(w, req, decision)

		return
	}

	out, charged, err := s.outbound(ctx, w, r, req, decision)
	if err != nil {
		s.log.Printf(req.Source, "proxy: %s %s %s:%d: %v", req.Source, clip(r.Method), req.Host, req.Port, err)
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})

		return
	}
	defer s.held.give(req.Source, charged)

	sw := &statusWriter{ResponseWriter: w}
	s.forward(req.Source).ServeHTTP(sw, out) //nolint:gosec // forwarding the guest's request is the job, and the director pinned where it dials
	s.cfg.Log.Printf("proxy: %s %s %s:%d %d", req.Source, clip(r.Method), req.Host, req.Port, sw.status)
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
	// Checked before the tls one, whose error names the host.
	if len(host) > maxHostLen {
		return Request{}, errors.New("the request names a host longer than a dns name")
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

// outbound builds the request the upstream sees, and returns the bytes it charged the source, for the caller to give back.
func (s *Server) outbound(ctx context.Context, w http.ResponseWriter, r *http.Request, req Request, decision Decision) (*http.Request, int, error) {
	held, rest, charged, err := s.hold(w, r, req.Source, decision.Hold)
	if err != nil {
		return nil, 0, err
	}

	out := r.Clone(context.WithValue(ctx, upstreamKey{}, decision.Upstream))
	out.RequestURI = ""
	out.URL.Scheme = "http"
	if req.TLS {
		out.URL.Scheme = "https"
	}
	out.URL.Host = req.Host

	// What the rewrite reserves stays charged with the body until the request is forwarded.
	reserve := func(n int) error {
		if err := s.held.reserve(req.Source, n); err != nil {
			return err
		}
		charged += n

		return nil
	}
	body, err := s.cfg.Director.Rewrite(out.Context(), req, out, held, reserve)
	if err != nil {
		s.held.give(req.Source, charged)

		return nil, 0, err
	}

	if rest != nil {
		out.Body = rest

		return out, charged, nil
	}
	if held == nil {
		return out, charged, nil
	}

	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.Header.Set("Content-Length", strconv.Itoa(len(body)))

	return out, charged, nil
}

// hold reads a body the director may put a secret in, under a deadline and the source's budget; past BodyCap the body streams on.
func (s *Server) hold(w http.ResponseWriter, r *http.Request, source netip.Addr, wanted bool) ([]byte, io.ReadCloser, int, error) {
	// A body that says it is past the cap can never take a secret, so it streams with nothing held.
	if !wanted || r.Body == nil || r.Body == http.NoBody || r.ContentLength > BodyCap {
		return nil, nil, 0, nil
	}

	control := http.NewResponseController(w)
	if err := control.SetReadDeadline(time.Now().Add(heldReadTimeout)); err != nil {
		return nil, nil, 0, fmt.Errorf("set the deadline of the request body: %w", err)
	}

	held, charged, err := s.read(r.Body, r.ContentLength, source)
	if err != nil {
		s.held.give(source, charged)

		return nil, nil, 0, err
	}
	// Only a read that ended clears its deadline: a stalled one keeps it, so net/http closes that connection rather than read on.
	if err := control.SetReadDeadline(time.Time{}); err != nil {
		s.held.give(source, charged)

		return nil, nil, 0, fmt.Errorf("clear the deadline of the request body: %w", err)
	}
	if len(held) <= BodyCap {
		return held, nil, charged, nil
	}

	return nil, &joinedBody{Reader: io.MultiReader(bytes.NewReader(held), r.Body), closer: r.Body}, charged, nil
}

// read fills a buffer to the body's length, or to BodyCap+1 when it names none; the budget was charged the buffer's whole capacity.
func (s *Server) read(body io.Reader, length int64, source netip.Addr) ([]byte, int, error) {
	limit, first := BodyCap+1, heldChunk
	if length >= 0 {
		limit, first = int(length), int(length)
	}

	var buf []byte
	for len(buf) < limit {
		grown, err := s.room(buf, first, limit, source)
		if err != nil {
			return nil, cap(buf), err
		}
		buf = grown

		n, err := body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, cap(buf), nil
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, cap(buf), fmt.Errorf("%w: it did not arrive within %s", errStalled, heldReadTimeout)
		}
		if err != nil {
			return nil, cap(buf), fmt.Errorf("read the request body: %w", err)
		}
	}

	return buf, cap(buf), nil
}

// room grows a full buffer toward limit, taking the growth from the source's budget before it allocates it.
func (s *Server) room(buf []byte, first, limit int, source netip.Addr) ([]byte, error) {
	if len(buf) < cap(buf) {
		return buf, nil
	}

	size := min(max(2*cap(buf), first), limit)
	if err := s.held.reserve(source, size-cap(buf)); err != nil {
		return nil, err
	}
	grown := make([]byte, len(buf), size)
	copy(grown, buf)

	return grown, nil
}

// statusOf tells a stalled body and a spent budget apart from the upstream failures every other error is.
func statusOf(err error) int {
	if errors.Is(err, errStalled) {
		return http.StatusRequestTimeout
	}
	if errors.Is(err, errOverBudget) {
		return http.StatusServiceUnavailable
	}

	return http.StatusBadGateway
}

// budget counts the bytes each source holds at once to put secrets in.
type budget struct {
	limit int

	mu sync.Mutex
	by map[netip.Addr]int
}

func (b *budget) reserve(source netip.Addr, n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.by[source]+n > b.limit {
		return fmt.Errorf("%w, a budget of %d bytes per sandbox; retry once one ends", errOverBudget, b.limit)
	}
	b.by[source] += n

	return nil
}

func (b *budget) give(source netip.Addr, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.by[source] -= n
	if b.by[source] <= 0 {
		delete(b.by, source)
	}
}

type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (j *joinedBody) Close() error { return j.closer.Close() }

func (s *Server) forward(source netip.Addr) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// The outbound request is already built, so the rewrite has nothing left to do.
		Rewrite:   func(*httputil.ProxyRequest) {},
		Transport: s.transport,
		ErrorLog:  s.log.Logger(func(string) netip.Addr { return source }),
		// The error can quote the rewritten request, which holds secret values, so neither the guest nor the log reads it (SHARD-299).
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the request to the upstream failed"})
		},
	}
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.log.Printf(sourceOf(r.RemoteAddr), "proxy: %s %s: %d %s", r.RemoteAddr, clip(r.Method), status, message)
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

// namedSource reads the address net/http names after "from " in its own line.
func namedSource(line string) netip.Addr {
	_, rest, ok := strings.Cut(line, " from ")
	if !ok {
		return netip.Addr{}
	}
	addr, _, _ := strings.Cut(rest, ": ")

	return sourceOf(addr)
}

// sourceOf is the address a connection came from, or the invalid address, whose log bound every unnamed source shares.
func sourceOf(remote string) netip.Addr {
	source, err := netip.ParseAddrPort(remote)
	if err != nil {
		return netip.Addr{}
	}

	return source.Addr().Unmap()
}

// clip bounds what a log line prints of a value the guest chose.
func clip(s string) string {
	if len(s) <= maxHostLen {
		return s
	}

	return s[:maxHostLen] + "..."
}

func canonicalHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}
