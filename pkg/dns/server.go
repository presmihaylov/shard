// Package dns is the resolver on the bridge gateway: it judges every question by name through a director
// before any upstream is asked, so a name the director refuses is never resolved and never carries a query out.
package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// Port is where the resolver listens on the gateway, over udp and tcp alike.
	Port = 53

	// maxMessage is the largest DNS message, which is what a tcp frame or an EDNS answer can carry.
	maxMessage = 65535
	// maxInflight bounds the handlers at once, so a query storm cannot hold a goroutine and a buffer per packet.
	maxInflight = 256

	upstreamTimeout = 5 * time.Second
	idleTimeout     = 5 * time.Second
)

// Question is one name a sandbox asked for, and the address it asked from.
type Question struct {
	Source netip.Addr
	// Name is lowercase and without the trailing dot, the way the proxy names a host.
	Name string
}

// Director says whether a question may be resolved; the resolver itself knows no policy.
type Director interface {
	Resolve(ctx context.Context, q Question) (bool, error)
}

// Config is what a Server is built from.
type Config struct {
	// Address is the gateway address both listeners bind, so only the bridge reaches them.
	Address netip.Addr
	// Upstreams are asked in turn for an allowed question, and the first that answers wins.
	Upstreams []netip.AddrPort
	Director  Director
	Log       *log.Logger
}

// Server answers udp and tcp questions from sandboxes with what the director allows.
type Server struct {
	cfg      Config
	inflight chan struct{}
	timeout  time.Duration
}

func New(cfg Config) (*Server, error) {
	if cfg.Director == nil || cfg.Log == nil {
		return nil, errors.New("the resolver needs a director and a log")
	}
	if !cfg.Address.IsValid() {
		return nil, errors.New("the resolver needs an address to listen on")
	}
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("the resolver needs an upstream to forward to")
	}

	return &Server{cfg: cfg, inflight: make(chan struct{}, maxInflight), timeout: upstreamTimeout}, nil
}

// Run listens on the port at the address, over udp and tcp, and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	address := netip.AddrPortFrom(s.cfg.Address, Port).String()

	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		return fmt.Errorf("listen for udp questions: %w", err)
	}

	tcp, err := net.Listen("tcp", address)
	if err != nil {
		return errors.Join(fmt.Errorf("listen for tcp questions: %w", err), udp.Close())
	}

	return s.Serve(ctx, udp, tcp)
}

// Serve runs the resolver over two listeners it then owns, so a test can hand it loopback ports.
func (s *Server) Serve(ctx context.Context, udp net.PacketConn, tcp net.Listener) error {
	errs := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Go(func() { errs <- s.serveUDP(ctx, &wg, udp) })
	wg.Go(func() { errs <- s.serveTCP(ctx, &wg, tcp) })

	var err error
	select {
	case <-ctx.Done():
	case err = <-errs:
	}

	// Closing both ends the other loop, and the handlers in flight end with the context they were given.
	closeErr := errors.Join(udp.Close(), tcp.Close())
	wg.Wait()

	return errors.Join(err, closeErr)
}

func (s *Server) serveUDP(ctx context.Context, wg *sync.WaitGroup, conn net.PacketConn) error {
	buf := make([]byte, maxMessage)
	for {
		n, from, err := conn.ReadFrom(buf)
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read a udp question: %w", err)
		}

		msg := make([]byte, n)
		copy(msg, buf[:n])

		if !s.spawn(ctx, wg, func() {
			answer, err := s.answer(ctx, from, msg, "udp")
			if err != nil {
				s.cfg.Log.Printf("dns: %s: %v", from, err)
			}
			if answer == nil {
				return
			}
			// The asker is the only one who could hear a write failure, so the log is where it goes.
			if _, err := conn.WriteTo(answer, from); err != nil {
				s.cfg.Log.Printf("dns: %s: write the answer: %v", from, err)
			}
		}) {
			return nil
		}
	}
}

func (s *Server) serveTCP(ctx context.Context, wg *sync.WaitGroup, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("accept a tcp question: %w", err)
		}

		if !s.spawn(ctx, wg, func() { s.serveConn(ctx, conn) }) {
			if err := conn.Close(); err != nil {
				s.cfg.Log.Printf("dns: %s: close: %v", conn.RemoteAddr(), err)
			}

			return nil
		}
	}
}

// spawn runs one handler under the in-flight bound, and says no once the context is done.
func (s *Server) spawn(ctx context.Context, wg *sync.WaitGroup, handle func()) bool {
	select {
	case s.inflight <- struct{}{}:
	case <-ctx.Done():
		return false
	}

	wg.Go(func() {
		defer func() { <-s.inflight }()
		handle()
	})

	return true
}

// serveConn answers every framed question on one tcp connection until the guest hangs up or goes quiet.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			s.cfg.Log.Printf("dns: %s: close: %v", conn.RemoteAddr(), err)
		}
	}()

	for {
		if err := conn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
			s.cfg.Log.Printf("dns: %s: %v", conn.RemoteAddr(), err)

			return
		}

		msg, err := readFramed(conn)
		// A hang-up and a quiet connection are how a tcp exchange ends, not faults.
		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
			return
		}
		if err != nil {
			s.cfg.Log.Printf("dns: %s: read a tcp question: %v", conn.RemoteAddr(), err)

			return
		}

		answer, err := s.answer(ctx, conn.RemoteAddr(), msg, "tcp")
		if err != nil {
			s.cfg.Log.Printf("dns: %s: %v", conn.RemoteAddr(), err)
		}
		if answer == nil {
			continue
		}

		if _, err := conn.Write(framed(answer)); err != nil {
			s.cfg.Log.Printf("dns: %s: write the answer: %v", conn.RemoteAddr(), err)

			return
		}
	}
}

// answer judges one message: the reply to send, nil when it gets none, and the fault to log, which never names the question.
func (s *Server) answer(ctx context.Context, from net.Addr, msg []byte, proto string) ([]byte, error) {
	var p dnsmessage.Parser

	header, err := p.Start(msg)
	if err != nil {
		return nil, fmt.Errorf("parse a question: %w", err)
	}
	// An answer sent to a server is not a question, and answering it would make a loop of two servers.
	if header.Response {
		return nil, nil
	}
	if header.OpCode != 0 {
		return reply(header, nil, dnsmessage.RCodeNotImplemented)
	}

	q, err := p.Question()
	if err != nil {
		return reply(header, nil, dnsmessage.RCodeFormatError)
	}
	// One question per message is what every resolver sends, and a second one has no name to judge by.
	if _, err := p.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return reply(header, &q, dnsmessage.RCodeFormatError)
	}

	source, err := sourceOf(from)
	if err != nil {
		return nil, err
	}

	allowed, err := s.cfg.Director.Resolve(ctx, Question{Source: source, Name: canonical(q.Name.String())})
	if err != nil {
		return servfail(header, &q, fmt.Errorf("judge a question: %w", err))
	}
	if !allowed {
		return reply(header, &q, dnsmessage.RCodeNameError)
	}

	answer, err := s.forward(ctx, proto, msg, header.ID)
	if err != nil {
		return servfail(header, &q, fmt.Errorf("forward a question: %w", err))
	}

	return answer, nil
}

// servfail pairs a fault with the reply that admits it, so the caller logs one and sends the other.
func servfail(header dnsmessage.Header, q *dnsmessage.Question, fault error) ([]byte, error) {
	out, err := reply(header, q, dnsmessage.RCodeServerFailure)

	return out, errors.Join(fault, err)
}

// forward sends the question to the first upstream that answers, over the protocol it came in on.
func (s *Server) forward(ctx context.Context, proto string, msg []byte, id uint16) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var errs []error
	for _, upstream := range s.cfg.Upstreams {
		answer, err := exchange(ctx, proto, upstream, msg)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", upstream, err))

			continue
		}

		var p dnsmessage.Parser
		header, err := p.Start(answer)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: parse the answer: %w", upstream, err))

			continue
		}
		if !header.Response || header.ID != id {
			errs = append(errs, fmt.Errorf("%s: the answer is not to the question sent", upstream))

			continue
		}

		return answer, nil
	}

	return nil, fmt.Errorf("no upstream answered: %w", errors.Join(errs...))
}

// exchange sends one message and reads one answer over a connection of its own, so answers never cross.
func exchange(ctx context.Context, proto string, upstream netip.AddrPort, msg []byte) (answer []byte, err error) {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, proto, upstream.String())
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	if proto == "tcp" {
		if _, err := conn.Write(framed(msg)); err != nil {
			return nil, err
		}

		return readFramed(conn)
	}

	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	buf := make([]byte, maxMessage)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}

	return buf[:n], nil
}

// reply answers without an upstream: the header and question echoed, since a stub refuses an NXDOMAIN that echoes nothing, and the code that says why.
func reply(header dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode) ([]byte, error) {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		OpCode:             header.OpCode,
		RecursionDesired:   header.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	})

	if err := b.StartQuestions(); err != nil {
		return nil, fmt.Errorf("build the reply: %w", err)
	}
	if q != nil {
		if err := b.Question(*q); err != nil {
			return nil, fmt.Errorf("build the reply: %w", err)
		}
	}

	out, err := b.Finish()
	if err != nil {
		return nil, fmt.Errorf("build the reply: %w", err)
	}

	return out, nil
}

// framed is the message behind the two-byte length tcp carries it under.
func framed(msg []byte) []byte {
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg))) //nolint:gosec // a DNS message never exceeds maxMessage
	copy(out[2:], msg)

	return out
}

func readFramed(r io.Reader) ([]byte, error) {
	var length [2]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}

	msg := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

func sourceOf(from net.Addr) (netip.Addr, error) {
	source, err := netip.ParseAddrPort(from.String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("the question came from %q, which is not an address", from)
	}

	return source.Addr().Unmap(), nil
}

func canonical(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}
