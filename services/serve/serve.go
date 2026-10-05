// Package serve is the TCP front of the daemon: it speaks plain HTTP behind a proxy that terminates TLS,
// verifies the JWT each request carries and then splices it onto the daemon's unix socket, byte for byte.
package serve

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/lograte"
	"github.com/presmihaylov/shard/services/api"
)

// DefaultListen is loopback, so only a proxy on the same host reaches the plain HTTP front.
const DefaultListen = "127.0.0.1:2376"

const (
	// headBytes bounds the request head the front reads before it decides, so no client grows one forever.
	headBytes = 64 * 1024
	// defaultHeadTimeout bounds a client that connects and then sends nothing.
	defaultHeadTimeout = 10 * time.Second
	// acceptBackoffMin and acceptBackoffMax bound the wait after an Accept that ran out of a resource, as net/http's do.
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
	// lingerFor bounds the drain before a close, as net/http's rstAvoidanceDelay does.
	lingerFor = 500 * time.Millisecond
)

// unauthorized is the whole answer to a request with no valid token: the socket is never dialed for it.
const unauthorized = `{"error":{"code":"unauthorized","message":"the bearer token is missing or invalid; send a valid token in Authorization: Bearer TOKEN"}}`

// unrouted is the answer to a route no scope reaches, an unknown one or a local one: the socket is never dialed for it.
const unrouted = `{"error":{"code":"forbidden","message":"no token scope reaches this route over the network; check the method and the path"}}`

// forbidden is the answer to a valid token whose scopes do not reach need: the socket is never dialed for it.
func forbidden(need api.Scope) string {
	return fmt.Sprintf(`{"error":{"code":"forbidden","message":"the token lacks %s; use a token with %s"}}`, need, need)
}

// badRequestLine is the answer to a request line net/http would not parse, so the front never checks a route the daemon reads otherwise.
const badRequestLine = `{"error":{"code":"invalid_request","message":"the request line does not parse"}}`

// Config is the wiring one front needs.
type Config struct {
	// Listen defaults to DefaultListen when empty.
	Listen string
	// SigningKeyFile holds the HS256 key that signs and checks every token; empty means <Root>/auth/signing-key, created on first use. Its value is never logged.
	SigningKeyFile string
	// Root is the daemon's state root, which is where the socket the front fronts sits.
	Root string
	Out  io.Writer
}

// Server is one front, over one signing key and one daemon socket.
type Server struct {
	listen      string
	socket      string
	signingKey  []byte
	tokens      *ledger
	caps        *capMux
	preAuth     *preAuth
	headTimeout time.Duration
	log         *log.Logger
	// refusals bounds the lines a flood past the pre-auth cap writes, per source.
	refusals *lograte.Log
}

// New loads the signing key and the ledger, so every reason to refuse is known before anything binds.
func New(cfg Config) (*Server, error) {
	if cfg.Root == "" {
		return nil, errors.New("shard serve needs a root: the daemon socket it fronts sits under it")
	}

	signingKey, keyPath, err := SigningKey(cfg.Root, cfg.SigningKeyFile)
	if err != nil {
		return nil, err
	}

	tokens, err := newLedger(TokensPath(keyPath))
	if err != nil {
		return nil, err
	}

	caps, err := newCapMux()
	if err != nil {
		return nil, err
	}

	listen := cfg.Listen
	if listen == "" {
		listen = DefaultListen
	}

	out := cfg.Out
	if out == nil {
		out = io.Discard
	}

	total, err := preAuthTotal()
	if err != nil {
		return nil, err
	}

	logger := log.New(out, "", log.LstdFlags)

	return &Server{
		listen:      listen,
		socket:      filepath.Join(cfg.Root, api.SocketFile),
		signingKey:  signingKey,
		tokens:      tokens,
		caps:        caps,
		preAuth:     newPreAuth(total, preAuthPerSource),
		headTimeout: defaultHeadTimeout,
		log:         logger,
		refusals:    lograte.New(logger, "serve"),
	}, nil
}

// Run binds the front and serves it until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	server, err := New(cfg)
	if err != nil {
		return err
	}

	listener, err := server.Listen()
	if err != nil {
		return err
	}

	server.log.Printf("serve listening on %s over plain http, in front of %s", listener.Addr(), server.socket)
	if !loopback(listener.Addr()) {
		server.log.Printf("serve: %s is not loopback, so a token crosses the network in clear text up to this port", listener.Addr())
	}

	return server.Serve(ctx, listener)
}

// Listen binds the TCP address. TLS is the job of the proxy in front.
func (s *Server) Listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", s.listen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.listen, err)
	}

	return listener, nil
}

// loopback reports whether addr is reachable from this host alone.
func loopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)

	return ok && tcp.IP.IsLoopback()
}

// Serve answers until ctx ends; any other end is the listener dying, an error so the unit restarts it.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	accepting := make(chan struct{})
	closed := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			closed <- listener.Close()
		case <-accepting:
			closed <- nil
		}
	}()

	err := s.accept(ctx, listener)
	close(accepting)

	if ctx.Err() == nil {
		return err
	}

	if err := <-closed; err != nil {
		return fmt.Errorf("close the front on %s: %w", s.listen, err)
	}

	return nil
}

// accept takes connections until the listener dies, and answers each within the pre-auth cap on its own goroutine.
func (s *Server) accept(ctx context.Context, listener net.Listener) error {
	var live sync.WaitGroup
	defer live.Wait()

	var backoff time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !exhausted(err) {
				return fmt.Errorf("accept on %s: %w", listener.Addr(), err)
			}
			backoff = s.waitOut(ctx, err, backoff)

			continue
		}
		backoff = 0

		source := sourceOf(conn.RemoteAddr())
		if !s.preAuth.enter(source) {
			s.refusals.Printf(source, "refused a connection from %s: it is past the cap on connections that show no token yet, %d in total and %d per source", source, s.preAuth.total, s.preAuth.perSource)
			if err := conn.Close(); !quiet(err) {
				s.log.Printf("close the connection from %s past the cap: %v", source, err)
			}

			continue
		}

		live.Go(func() { s.handle(ctx, conn, sync.OnceFunc(func() { s.preAuth.leave(source) })) })
	}
}

// exhausted reports an Accept error that a connection closing elsewhere cures, which net/http waits out rather than dies on.
func exhausted(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM)
}

// waitOut sleeps the next step of the backoff after an Accept that ran out of a resource, and answers that step.
func (s *Server) waitOut(ctx context.Context, err error, last time.Duration) time.Duration {
	next := min(max(2*last, acceptBackoffMin), acceptBackoffMax)
	s.log.Printf("%v; accepting again in %s", err, next)

	select {
	case <-ctx.Done():
	case <-time.After(next):
	}

	return next
}

// handle checks the token, then stops reading: the rest is bytes both ways. leave frees the pre-auth slot.
func (s *Server) handle(ctx context.Context, conn net.Conn, leave func()) {
	defer leave()

	closeConn := func() {
		if err := conn.Close(); !quiet(err) {
			s.log.Printf("close the connection from %s: %v", conn.RemoteAddr(), err)
		}
	}
	defer closeConn()

	// A front that is ending must not wait for a client that holds an idle connection open.
	defer context.AfterFunc(ctx, closeConn)()

	head, err := s.readHead(conn)
	if err != nil {
		s.log.Printf("read the request from %s: %v", conn.RemoteAddr(), err)

		return
	}

	method, target, ok := requestLine(head)
	if !ok {
		s.reject(conn)

		return
	}

	token, ok, forbid, reason := s.authorize(head, method, target)
	if !ok {
		if forbid {
			s.forbid(conn, token.Subject, api.Scope(reason))

			return
		}
		s.refuse(conn, reason)

		return
	}
	s.log.Printf("authorized %s as %s", conn.RemoteAddr(), token.Subject)
	head = stampScopes(head, token.Scopes)
	ctx, stopGuard := s.guard(ctx, token)
	defer stopGuard()
	defer context.AfterFunc(ctx, closeConn)()
	// A logs -f or an exec attach holds its connection for long, and a valid client must not be refused for that.
	leave()

	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	if err != nil {
		s.log.Printf("dial the daemon socket %s for %s: %v", s.socket, conn.RemoteAddr(), err)
		s.answer(conn, "502 Bad Gateway", `{"error":{"code":"internal","message":"the shard daemon does not answer on its socket"}}`)

		return
	}
	closeUpstream := func() {
		if err := upstream.Close(); !quiet(err) {
			s.log.Printf("close the daemon socket for %s: %v", conn.RemoteAddr(), err)
		}
	}
	defer closeUpstream()
	defer context.AfterFunc(ctx, closeUpstream)()

	if err := errors.Join(s.proxy(conn, upstream, head), context.Cause(ctx)); err != nil {
		s.log.Printf("proxy the connection from %s: %v", conn.RemoteAddr(), err)
	}
}

// readHead reads up to the blank line that ends the headers, which is all the front ever parses.
func (s *Server) readHead(conn net.Conn) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(s.headTimeout)); err != nil {
		return nil, fmt.Errorf("set the deadline of the request head: %w", err)
	}

	head, err := readHead(conn)
	if err != nil {
		return nil, err
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear the deadline of the request head: %w", err)
	}

	return head, nil
}

// readHead reads a byte at a time because a buffered reader would swallow body bytes past the blank line, which the splice would then lose.
func readHead(r io.Reader) ([]byte, error) {
	head := make([]byte, 0, 1024)
	one := make([]byte, 1)

	for {
		n, err := r.Read(one)
		if n > 0 {
			// A bare LF ends the head for the daemon but not for this scan, so it hides a smuggled request the scope check never sees; refuse it.
			if one[0] == '\n' && !bytes.HasSuffix(head, []byte("\r")) {
				return nil, errors.New("the request head has a line feed with no carriage return")
			}
			head = append(head, one[0])
			if bytes.HasSuffix(head, []byte("\r\n\r\n")) {
				return head, nil
			}
			if len(head) >= headBytes {
				return nil, fmt.Errorf("the request head is longer than %d bytes", headBytes)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("read the request head: %w", err)
		}
	}
}

// No daemon connection opens until the token, ledger and route scope all permit it; reason is the cause a 401 logs or the scope a 403 needs.
func (s *Server) authorize(head []byte, method string, target *url.URL) (claims, bool, bool, string) {
	fields, ok := headerFields(head)
	if !ok {
		return claims{}, false, false, "no valid token"
	}

	scheme, token, found := strings.Cut(fields.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return claims{}, false, false, "no valid token"
	}

	c, err := verify(s.signingKey, strings.TrimSpace(token))
	if err != nil {
		return claims{}, false, false, "no valid token"
	}

	if err := s.tokens.refresh(); err != nil {
		s.log.Printf("read the ledger: %v", err)

		return c, false, false, "the ledger is unavailable"
	}
	entry, known := s.tokens.lookup(c.ID)
	if !known {
		return c, false, false, "the token id is not in the ledger"
	}
	if entry.Revoked {
		return c, false, false, "the token is revoked"
	}

	need, known := s.caps.scope(method, target)
	if !known || !covers(c.Scopes, need) {
		return c, false, true, string(need)
	}

	return c, true, false, ""
}

// requestLine parses the method and the target as net/http does, on the ASCII space alone, so the front checks the route the daemon serves.
func requestLine(head []byte) (string, *url.URL, bool) {
	line, _, found := bytes.Cut(head, []byte("\r\n"))
	if !found {
		return "", nil, false
	}

	method, rest, found := strings.Cut(string(line), " ")
	uri, proto, both := strings.Cut(rest, " ")
	if !found || !both || !validMethod(method) {
		return "", nil, false
	}
	if _, _, ok := http.ParseHTTPVersion(proto); !ok {
		return "", nil, false
	}

	// A CONNECT to a host:port is an authority alone, which net/http parses behind a scheme it then drops.
	authority := method == http.MethodConnect && !strings.HasPrefix(uri, "/")
	if authority {
		uri = "http://" + uri
	}
	target, err := url.ParseRequestURI(uri)
	if err != nil {
		return "", nil, false
	}
	if authority {
		target.Scheme = ""
	}

	return method, target, true
}

// methodChars are the bytes RFC 9110 allows in a method, the set net/http checks.
const methodChars = "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// validMethod reports whether method is an RFC 9110 token: Trim leaves nothing only when every byte is in the set.
func validMethod(method string) bool {
	return method != "" && strings.Trim(method, methodChars) == ""
}

// reject answers 400 and closes. The daemon would refuse the line too, so nothing is dialed.
func (s *Server) reject(conn net.Conn) {
	s.log.Printf("rejected the connection from %s: the request line does not parse", conn.RemoteAddr())
	s.answer(conn, "400 Bad Request", badRequestLine)
}

// refuse answers 401 and closes. Nothing is dialed, so a request with no valid token never reaches the daemon.
func (s *Server) refuse(conn net.Conn, reason string) {
	s.log.Printf("refused the connection from %s: %s", conn.RemoteAddr(), reason)
	s.answer(conn, "401 Unauthorized", unauthorized)
}

// forbid answers 403 and closes. The token is valid but lacks need, or no scope reaches the route, so nothing is dialed.
func (s *Server) forbid(conn net.Conn, sub string, need api.Scope) {
	if need == "" {
		s.log.Printf("forbade %s as %s: no scope reaches the route", conn.RemoteAddr(), sub)
		s.answer(conn, "403 Forbidden", unrouted)

		return
	}
	s.log.Printf("forbade %s as %s: the route needs %s", conn.RemoteAddr(), sub, need)
	s.answer(conn, "403 Forbidden", forbidden(need))
}

func (s *Server) answer(conn net.Conn, status, body string) {
	head := fmt.Sprintf("HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", status, len(body)+1)
	if _, err := io.WriteString(conn, head+body+"\n"); !quiet(err) {
		s.log.Printf("answer %s to %s: %v", status, conn.RemoteAddr(), err)

		return
	}

	if err := closeWrite(conn); err != nil {
		s.log.Printf("end the answer %s to %s: %v", status, conn.RemoteAddr(), err)

		return
	}
	if err := linger(conn); err != nil {
		s.log.Printf("after the answer %s to %s: %v", status, conn.RemoteAddr(), err)
	}
}

// headerFields parses the head into the request headers, so the front reads the ones it needs.
func headerFields(head []byte) (textproto.MIMEHeader, bool) {
	headers := textproto.NewReader(bufio.NewReader(bytes.NewReader(head)))
	if _, err := headers.ReadLine(); err != nil {
		return nil, false
	}

	fields, err := headers.ReadMIMEHeader()
	if err != nil {
		return nil, false
	}

	return fields, true
}

// proxy replays the head and copies both ways. A handshake keeps its own Connection header so the daemon
// owns that connection; every other request is forced closed, so the front checks the next one too.
func (s *Server) proxy(client, upstream net.Conn, head []byte) error {
	if isHandshake(head) {
		return spliceUpgrade(client, upstream, head)
	}

	return splice(client, upstream, setConnectionClose(head))
}

// isHandshake reports whether the head is the WebSocket opening handshake, the one request the front
// does not force closed. It mirrors isHandshake() in services/api, so the front and the daemon agree on
// what an upgrade is: a lone Upgrade header is not enough to skip the Connection: close rewrite.
func isHandshake(head []byte) bool {
	fields, ok := headerFields(head)
	if !ok {
		return false
	}

	return hasToken(fields.Get("Connection"), "upgrade") &&
		hasToken(fields.Get("Upgrade"), "websocket") &&
		fields.Get("Sec-WebSocket-Version") == "13" &&
		fields.Get("Sec-WebSocket-Key") != ""
}

// hasToken reports whether a comma-separated header names token, in any case.
func hasToken(header, token string) bool {
	for part := range strings.SplitSeq(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}

	return false
}

// stampScopes drops any X-Shard-Scopes the client sent and appends the token's scopes, so the daemon trusts only the front's copy and a forged header can remove rights, never add them.
func stampScopes(head []byte, scopes []string) []byte {
	trimmed := bytes.TrimSuffix(head, []byte("\r\n\r\n"))
	lines := bytes.Split(trimmed, []byte("\r\n"))

	kept := lines[:1] // the request line carries no header name.
	for _, line := range lines[1:] {
		if hasHeaderName(line, api.ScopesHeader) {
			continue
		}

		kept = append(kept, line)
	}
	kept = append(kept, []byte(api.ScopesHeader+": "+strings.Join(scopes, ",")))

	return append(bytes.Join(kept, []byte("\r\n")), "\r\n\r\n"...)
}

// setConnectionClose drops any Connection header the client sent and appends Connection: close.
func setConnectionClose(head []byte) []byte {
	trimmed := bytes.TrimSuffix(head, []byte("\r\n\r\n"))
	lines := bytes.Split(trimmed, []byte("\r\n"))

	kept := lines[:1] // the request line carries no header name.
	for _, line := range lines[1:] {
		if hasHeaderName(line, "Connection") {
			continue
		}

		kept = append(kept, line)
	}
	kept = append(kept, []byte("Connection: close"))

	return append(bytes.Join(kept, []byte("\r\n")), "\r\n\r\n"...)
}

// hasHeaderName reports whether a header line names field, whatever its case and whatever its value.
func hasHeaderName(line []byte, field string) bool {
	name, _, found := bytes.Cut(line, []byte(":"))
	if !found {
		return false
	}

	return strings.EqualFold(strings.TrimSpace(string(name)), field)
}

// splice replays the head the front read and then copies both ways until the daemon's answer ends.
func splice(client, upstream net.Conn, head []byte) error {
	if _, err := upstream.Write(head); err != nil {
		return fmt.Errorf("replay the request head onto the daemon socket: %w", err)
	}

	return copyBothWays(client, upstream)
}

// spliceUpgrade replays a handshake and reads the daemon's status line first. A 101 becomes the two-way
// copy; any other answer ends after that one response, so no request pipelined behind it reaches the daemon.
func spliceUpgrade(client, upstream net.Conn, head []byte) error {
	if _, err := upstream.Write(head); err != nil {
		return fmt.Errorf("replay the upgrade head onto the daemon socket: %w", err)
	}

	status, err := readStatusLine(upstream)
	if err != nil {
		return err
	}
	if _, err := client.Write(status); err != nil {
		return fmt.Errorf("relay the response status line to the client: %w", err)
	}

	if isSwitchingProtocols(status) {
		return copyBothWays(client, upstream)
	}

	return endOneResponse(client, upstream)
}

// endOneResponse relays the rest of a single daemon response and ends. It half-closes the send side, so the
// daemon reads EOF and closes, and it never forwards the client, so a pipelined request cannot reach the daemon.
func endOneResponse(client, upstream net.Conn) error {
	if err := closeWrite(upstream); err != nil {
		return fmt.Errorf("half-close the daemon socket after a non-101 answer: %w", err)
	}

	if err := forward(client, upstream); err != nil {
		return err
	}

	return linger(client)
}

// linger drops what the client still sends until it closes or lingerFor passes, so an unread request does not make the close a reset that destroys the answer.
func linger(conn net.Conn) error {
	if err := conn.SetReadDeadline(time.Now().Add(lingerFor)); err != nil {
		return fmt.Errorf("set the deadline of the drain: %w", err)
	}
	if _, err := io.Copy(io.Discard, conn); !quiet(err) {
		return fmt.Errorf("drain the client before the close: %w", err)
	}

	return nil
}

// closeWrite half-closes conn when it can, so the far end reads EOF.
func closeWrite(conn net.Conn) error {
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return nil
	}
	if err := half.CloseWrite(); !quiet(err) {
		return err
	}

	return nil
}

// copyBothWays copies each direction until the daemon's answer ends, then ends the other copier's read and drains the client.
func copyBothWays(client, upstream net.Conn) error {
	sent := make(chan error, 1)
	go func() { sent <- forward(upstream, client) }()

	received := forward(client, upstream)

	// The other copier blocks on a client with nothing more to say, so its read ends here.
	if err := client.SetReadDeadline(time.Now()); err != nil {
		return errors.Join(received, fmt.Errorf("end the read of %s: %w", client.RemoteAddr(), err))
	}
	ended := <-sent

	// A copier the deadline beat to a pipelined request leaves it unread, and a close over unread bytes is a reset.
	return errors.Join(received, ended, linger(client))
}

// readStatusLine reads the daemon's response status line one byte at a time, so nothing past it is consumed and the splice that may follow loses no bytes.
func readStatusLine(r io.Reader) ([]byte, error) {
	line := make([]byte, 0, 64)
	one := make([]byte, 1)

	for {
		n, err := r.Read(one)
		if n > 0 {
			line = append(line, one[0])
			if bytes.HasSuffix(line, []byte("\n")) {
				return line, nil
			}
			if len(line) >= headBytes {
				return nil, fmt.Errorf("the response status line is longer than %d bytes", headBytes)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("read the response status line: %w", err)
		}
	}
}

// isSwitchingProtocols reports whether the status line is a 101, the one answer that keeps the connection.
func isSwitchingProtocols(status []byte) bool {
	fields := bytes.Fields(status)

	return len(fields) >= 2 && string(fields[1]) == "101"
}

// forward copies one direction and half-closes the far end, so the side that reads sees the end of it.
func forward(dst, src net.Conn) error {
	_, err := io.Copy(dst, src)
	if quiet(err) {
		err = nil
	}

	return errors.Join(err, closeWrite(dst))
}

// quiet reports the ends that are how a proxied connection stops, rather than a failure to report.
func quiet(err error) bool {
	if err == nil {
		return true
	}

	// Darwin answers a write to a unix socket whose peer shut down with ENOTCONN where Linux answers EPIPE (SHARD-626).
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ENOTCONN)
}
