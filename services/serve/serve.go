// Package serve is the TCP front of the daemon: it terminates TLS, verifies the JWT each request
// carries and then splices it onto the daemon's unix socket, byte for byte.
package serve

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/services/api"
)

// DefaultListen is what the front binds when no address is named, as dockerd's TLS port.
const DefaultListen = ":2376"

const (
	// headBytes bounds the request head the front reads before it decides, so no client grows one forever.
	headBytes = 64 * 1024
	// headTimeout bounds a client that connects and then sends nothing.
	headTimeout = 10 * time.Second
)

// unauthorized is the whole answer to a request with no valid token: the socket is never dialed for it.
const unauthorized = `{"error":{"code":"unauthorized","message":"the request carries no valid bearer token"}}`

// forbidden is the answer to a valid token whose scopes do not reach the route: the socket is never dialed for it.
const forbidden = `{"error":{"code":"forbidden","message":"the token does not carry a scope for this route"}}`

// Config is the wiring one front needs.
type Config struct {
	// Listen defaults to DefaultListen when empty.
	Listen string
	// CertFile and KeyFile are the TLS pair. Without both the front refuses to start; it never serves plain tcp.
	CertFile string
	KeyFile  string
	// SecretFile holds the HS256 secret that signs and checks every token. Its value is never logged.
	SecretFile string
	// Root is the daemon's state root, which is where the socket the front fronts sits.
	Root string
	Out  io.Writer
}

// Server is one front, over one secret and one daemon socket.
type Server struct {
	listen string
	socket string
	secret []byte
	caps   *capMux
	tls    *tls.Config
	log    *log.Logger
}

// New reads the secret and the TLS pair, so every reason to refuse is known before anything binds.
func New(cfg Config) (*Server, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("shard serve needs --cert and --key: it terminates tls and never accepts plain tcp")
	}
	if cfg.Root == "" {
		return nil, errors.New("shard serve needs a root: the daemon socket it fronts sits under it")
	}

	secret, err := ReadSecret(cfg.SecretFile)
	if err != nil {
		return nil, err
	}

	caps, err := newCapMux()
	if err != nil {
		return nil, err
	}

	pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the certificate %s and the key %s: %w", cfg.CertFile, cfg.KeyFile, err)
	}

	listen := cfg.Listen
	if listen == "" {
		listen = DefaultListen
	}

	out := cfg.Out
	if out == nil {
		out = io.Discard
	}

	return &Server{
		listen: listen,
		socket: filepath.Join(cfg.Root, api.SocketFile),
		secret: secret,
		caps:   caps,
		tls:    &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
		log:    log.New(out, "", log.LstdFlags),
	}, nil
}

// ReadToken refuses a file others can read, because the token is the whole of the authentication.
func ReadToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("shard needs --token-file: every request to a shard serve front carries a bearer token")
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read the token file %s: %w", path, err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("the token file %s is at mode %04o, which everyone on the host can read", path, info.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the token file %s: %w", path, err)
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the token file %s holds no token", path)
	}

	// serve mint writes a JSON record; --token-file takes it whole or the bare token.
	if strings.HasPrefix(token, "{") {
		var record struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(token), &record); err != nil {
			return "", fmt.Errorf("parse the mint record in the token file %s: %w", path, err)
		}
		token = strings.TrimSpace(record.Token)
		if token == "" {
			return "", fmt.Errorf("the mint record in the token file %s holds no token", path)
		}
	}

	return token, nil
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

	server.log.Printf("serve listening on %s over tls, in front of %s", listener.Addr(), server.socket)

	return server.Serve(ctx, listener)
}

// Listen binds the TCP address under TLS. There is no plaintext listener to bind.
func (s *Server) Listen() (net.Listener, error) {
	listener, err := tls.Listen("tcp", s.listen, s.tls)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.listen, err)
	}

	return listener, nil
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

// accept takes connections until the listener dies, and answers each on its own goroutine.
func (s *Server) accept(ctx context.Context, listener net.Listener) error {
	var live sync.WaitGroup
	defer live.Wait()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("accept on %s: %w", listener.Addr(), err)
		}

		live.Go(func() { s.handle(ctx, conn) })
	}
}

// handle checks the token, then stops reading: the rest is bytes both ways, WebSocket included.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
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

	sub, ok, forbid := s.authorize(head)
	if !ok {
		if forbid {
			s.forbid(conn, sub)

			return
		}
		s.refuse(conn)

		return
	}
	s.log.Printf("authorized %s as %s", conn.RemoteAddr(), sub)

	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	if err != nil {
		s.log.Printf("dial the daemon socket %s for %s: %v", s.socket, conn.RemoteAddr(), err)
		s.answer(conn, "502 Bad Gateway", `{"error":{"code":"internal","message":"the shard daemon does not answer on its socket"}}`)

		return
	}
	defer func() {
		if err := upstream.Close(); !quiet(err) {
			s.log.Printf("close the daemon socket for %s: %v", conn.RemoteAddr(), err)
		}
	}()

	if err := s.proxy(conn, upstream, head); err != nil {
		s.log.Printf("proxy the connection from %s: %v", conn.RemoteAddr(), err)
	}
}

// readHead reads up to the blank line that ends the headers, which is all the front ever parses.
func (s *Server) readHead(conn net.Conn) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(headTimeout)); err != nil {
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

// authorize verifies the token and checks its scopes reach the route; nothing is dialed without both.
// It answers the subject, whether the request is authorized, and, when it is not, whether that is a 403.
func (s *Server) authorize(head []byte) (string, bool, bool) {
	fields, ok := headerFields(head)
	if !ok {
		return "", false, false
	}

	scheme, token, found := strings.Cut(fields.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false, false
	}

	sub, scopes, err := verify(s.secret, strings.TrimSpace(token))
	if err != nil {
		return "", false, false
	}

	method, target, ok := requestLine(head)
	if !ok {
		return "", false, false
	}

	need, known := s.caps.capability(method, target)
	if !known || !covers(scopes, need) {
		return sub, false, true
	}

	return sub, true, false
}

// requestLine parses the method and the target of the head, so the front can find the route's capability.
func requestLine(head []byte) (string, *url.URL, bool) {
	line, _, found := bytes.Cut(head, []byte("\r\n"))
	if !found {
		return "", nil, false
	}

	parts := bytes.Fields(line)
	if len(parts) < 2 {
		return "", nil, false
	}

	target, err := url.ParseRequestURI(string(parts[1]))
	if err != nil {
		return "", nil, false
	}

	return string(parts[0]), target, true
}

// refuse answers 401 and closes. Nothing is dialed, so a request with no token never reaches the daemon.
func (s *Server) refuse(conn net.Conn) {
	s.log.Printf("refused the connection from %s: no valid token", conn.RemoteAddr())
	s.answer(conn, "401 Unauthorized", unauthorized)
}

// forbid answers 403 and closes. The token is valid but carries no scope for this route, so nothing is dialed.
func (s *Server) forbid(conn net.Conn, sub string) {
	s.log.Printf("forbade %s as %s: no scope for the route", conn.RemoteAddr(), sub)
	s.answer(conn, "403 Forbidden", forbidden)
}

func (s *Server) answer(conn net.Conn, status, body string) {
	head := fmt.Sprintf("HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", status, len(body)+1)
	if _, err := io.WriteString(conn, head+body+"\n"); !quiet(err) {
		s.log.Printf("answer %s to %s: %v", status, conn.RemoteAddr(), err)
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
// does not force closed. It mirrors handshake() in services/api, so the front and the daemon agree on
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
// daemon reads EOF and closes, and it never reads the client, so a pipelined request cannot reach the daemon.
func endOneResponse(client, upstream net.Conn) error {
	if half, ok := upstream.(interface{ CloseWrite() error }); ok {
		if err := half.CloseWrite(); !quiet(err) {
			return fmt.Errorf("half-close the daemon socket after a non-101 answer: %w", err)
		}
	}

	return forward(client, upstream)
}

// copyBothWays copies each direction until the daemon's answer ends, then ends the other copier's read.
func copyBothWays(client, upstream net.Conn) error {
	sent := make(chan error, 1)
	go func() { sent <- forward(upstream, client) }()

	received := forward(client, upstream)

	// The other copier blocks on a client with nothing more to say, so its read ends here.
	if err := client.SetReadDeadline(time.Now()); err != nil {
		return errors.Join(received, fmt.Errorf("end the read of %s: %w", client.RemoteAddr(), err))
	}

	return errors.Join(received, <-sent)
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

	half, ok := dst.(interface{ CloseWrite() error })
	if !ok {
		return err
	}

	closed := half.CloseWrite()
	if quiet(closed) {
		closed = nil
	}

	return errors.Join(err, closed)
}

// quiet reports the ends that are how a proxied connection stops, rather than a failure to report.
func quiet(err error) bool {
	if err == nil {
		return true
	}

	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}
