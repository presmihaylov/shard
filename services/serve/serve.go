// Package serve is the TCP front of the daemon: it terminates TLS, checks the bearer token of a
// connection and then splices it onto the daemon's unix socket, byte for byte.
package serve

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/textproto"
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
const unauthorized = `{"error":"the request carries no valid bearer token"}`

// Config is the wiring one front needs.
type Config struct {
	// Listen defaults to DefaultListen when empty.
	Listen string
	// CertFile and KeyFile are the TLS pair. Without both the front refuses to start; it never serves plain tcp.
	CertFile string
	KeyFile  string
	// TokenFile holds the bearer token every request carries. Its value is never logged.
	TokenFile string
	// Root is the daemon's state root, which is where the socket the front fronts sits.
	Root string
	Out  io.Writer
}

// Server is one front, over one token and one daemon socket.
type Server struct {
	listen string
	socket string
	token  string
	tls    *tls.Config
	log    *log.Logger
}

// New reads the token and the TLS pair, so every reason to refuse is known before anything binds.
func New(cfg Config) (*Server, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("shard serve needs --cert and --key: it terminates tls and never accepts plain tcp")
	}
	if cfg.Root == "" {
		return nil, errors.New("shard serve needs a root: the daemon socket it fronts sits under it")
	}

	token, err := ReadToken(cfg.TokenFile)
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
		token:  token,
		tls:    &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
		log:    log.New(out, "", log.LstdFlags),
	}, nil
}

// ReadToken reads the token a front checks and a client sends. It refuses a file others can read,
// because the token is the whole of the authentication, and it never reports the value.
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

// handle checks the token of one connection and then stops reading it: the rest is bytes both ways,
// so an exec upgrade and a logs follow pass through untouched.
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

	if !s.authorized(head) {
		s.refuse(conn)

		return
	}

	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", s.socket)
	if err != nil {
		s.log.Printf("dial the daemon socket %s for %s: %v", s.socket, conn.RemoteAddr(), err)
		s.answer(conn, "502 Bad Gateway", `{"error":"the shard daemon does not answer on its socket"}`)

		return
	}
	defer func() {
		if err := upstream.Close(); !quiet(err) {
			s.log.Printf("close the daemon socket for %s: %v", conn.RemoteAddr(), err)
		}
	}()

	if err := splice(conn, upstream, head); err != nil {
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

// authorized compares the bearer token in constant time, so no answer of this front times a guess.
func (s *Server) authorized(head []byte) bool {
	headers := textproto.NewReader(bufio.NewReader(bytes.NewReader(head)))

	if _, err := headers.ReadLine(); err != nil {
		return false
	}

	fields, err := headers.ReadMIMEHeader()
	if err != nil {
		return false
	}

	scheme, token, found := strings.Cut(fields.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(s.token)) == 1
}

// refuse answers 401 and closes. Nothing is dialed, so a request with no token never reaches the daemon.
func (s *Server) refuse(conn net.Conn) {
	s.log.Printf("refused the connection from %s: the bearer token does not match", conn.RemoteAddr())
	s.answer(conn, "401 Unauthorized", unauthorized)
}

func (s *Server) answer(conn net.Conn, status, body string) {
	head := fmt.Sprintf("HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", status, len(body)+1)
	if _, err := io.WriteString(conn, head+body+"\n"); !quiet(err) {
		s.log.Printf("answer %s to %s: %v", status, conn.RemoteAddr(), err)
	}
}

// splice replays the head the front read and then copies both ways until the daemon's answer ends.
func splice(client, upstream net.Conn, head []byte) error {
	if _, err := upstream.Write(head); err != nil {
		return fmt.Errorf("replay the request head onto the daemon socket: %w", err)
	}

	sent := make(chan error, 1)
	go func() { sent <- forward(upstream, client) }()

	received := forward(client, upstream)

	// The other copier blocks on a client with nothing more to say, so its read ends here.
	if err := client.SetReadDeadline(time.Now()); err != nil {
		return errors.Join(received, fmt.Errorf("end the read of %s: %w", client.RemoteAddr(), err))
	}

	return errors.Join(received, <-sent)
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
