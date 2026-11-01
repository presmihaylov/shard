// Package sshd serves ssh connections into sandboxes over the exec path, with one host key for the whole daemon.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

const (
	// handshakeBudget bounds a client that opened the stream and never finished the key exchange.
	handshakeBudget = 30 * time.Second
	// maxSessions is OpenSSH's own MaxSessions default; every session also takes one of the sandbox's exec slots.
	maxSessions = 10
	maxForwards = 64
)

// Sandboxes is the part of the sandbox service an ssh connection drives.
type Sandboxes interface {
	CreateExec(ctx context.Context, ref string, req sandbox.ExecRequest) (models.Exec, error)
	Attach(ctx context.Context, ref, execID string, streams sandbox.Streams) (sandbox.Attached, error)
	ResizeExec(ctx context.Context, ref, execID string, size sandbox.TerminalSize) error
	KillExec(ctx context.Context, ref, execID, signal string) error
	WaitExec(ctx context.Context, ref, execID string) (models.Exec, error)
	DeleteExec(ctx context.Context, ref, execID string) error
	DialPort(ctx context.Context, ref string, port uint16) (net.Conn, error)
}

// Server answers ssh connections, each into the one sandbox its caller names.
type Server struct {
	sandboxes Sandboxes
	signer    ssh.Signer
	config    *ssh.ServerConfig
	report    func(string)
}

// New makes a server that presents signer and reports what went wrong after a client could be told.
func New(signer ssh.Signer, sandboxes Sandboxes, report func(string)) *Server {
	// The route that hands a connection over checked the token already, so ssh itself asks for nothing.
	config := &ssh.ServerConfig{NoClientAuth: true, ServerVersion: "SSH-2.0-shard"}
	config.AddHostKey(signer)

	return &Server{sandboxes: sandboxes, signer: signer, config: config, report: report}
}

// HostKey is the host key as one authorized_keys line, which a client pins in its known_hosts.
func (s *Server) HostKey() string {
	line := ssh.MarshalAuthorizedKey(s.signer.PublicKey())

	return string(line[:len(line)-1])
}

// Options is what one connection reaches and how its refusals read.
type Options struct {
	// Sandbox is the sandbox every session and forward of the connection lands in.
	Sandbox string
	// Explain is the text a refusal shows the client, so a remote one reads no host detail.
	Explain func(error) string
}

// connection is one client's ssh connection and the channels it opened.
type connection struct {
	srv  *Server
	opts Options
	user string
	ctx  context.Context

	sessions chan struct{}
	forwards chan struct{}
	// noMoreSessions is the client's promise that it opens no other session, which a later open breaks.
	noMoreSessions atomic.Bool
}

// Serve speaks ssh over conn until the client leaves or ctx ends, and returns once every channel it opened has ended.
func (s *Server) Serve(ctx context.Context, conn net.Conn, opts Options) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	closed := make(chan error, 1)
	// Closing the stream is what ends the reads the library and every channel wait in.
	context.AfterFunc(ctx, func() { closed <- conn.Close() })
	defer func() {
		cancel()
		err = errors.Join(err, quiet(<-closed))
	}()

	if err := conn.SetDeadline(time.Now().Add(handshakeBudget)); err != nil {
		return fmt.Errorf("bound the ssh handshake: %w", err)
	}
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		return fmt.Errorf("ssh handshake: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return errors.Join(fmt.Errorf("lift the ssh handshake bound: %w", err), quiet(sconn.Close()))
	}

	c := &connection{
		srv:      s,
		opts:     opts,
		user:     sconn.User(),
		ctx:      ctx,
		sessions: make(chan struct{}, maxSessions),
		forwards: make(chan struct{}, maxForwards),
	}

	var wg sync.WaitGroup
	wg.Go(func() { c.globalRequests(reqs) })
	for ch := range chans {
		c.open(ch, &wg)
	}
	// The library closed the channel list, so the connection is gone; the cancel ends every attach still waiting.
	cancel()
	wg.Wait()

	return nil
}

func (c *connection) open(ch ssh.NewChannel, wg *sync.WaitGroup) {
	switch ch.ChannelType() {
	case "session":
		c.openSession(ch, wg)
	case "direct-tcpip":
		c.openForward(ch, wg)
	default:
		c.reject(ch, ssh.UnknownChannelType, fmt.Sprintf("shard opens no %s channel: it serves sessions and -L forwards to the sandbox's loopback", ch.ChannelType()))
	}
}

func (c *connection) openSession(ch ssh.NewChannel, wg *sync.WaitGroup) {
	if c.noMoreSessions.Load() {
		c.reject(ch, ssh.Prohibited, "this connection promised no more sessions")

		return
	}
	if !acquire(c.sessions) {
		c.reject(ch, ssh.ResourceShortage, fmt.Sprintf("this connection has %d sessions open already", maxSessions))

		return
	}

	channel, reqs, err := ch.Accept()
	if err != nil {
		<-c.sessions
		c.srv.report(fmt.Sprintf("ssh: sandbox %s: accept a session: %v", c.opts.Sandbox, err))

		return
	}
	wg.Go(func() {
		defer func() { <-c.sessions }()
		c.serveSession(channel, reqs)
	})
}

func (c *connection) reject(ch ssh.NewChannel, reason ssh.RejectionReason, message string) {
	if err := ch.Reject(reason, message); err != nil {
		c.srv.report(fmt.Sprintf("ssh: sandbox %s: refuse a %s channel: %v", c.opts.Sandbox, ch.ChannelType(), err))
	}
}

// globalRequests refuses every remote forward, and answers the rest as OpenSSH does, until the connection ends.
func (c *connection) globalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type == "no-more-sessions@openssh.com" {
			c.noMoreSessions.Store(true)
		}
		// sshd fails keepalive@openssh.com too, and the client takes either answer as the server being alive.
		if err := req.Reply(false, nil); err != nil {
			c.srv.report(fmt.Sprintf("ssh: sandbox %s: answer a %s request: %v", c.opts.Sandbox, req.Type, err))
		}
	}
}

func acquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// quiet drops the error of a stream another side closed first.
func quiet(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}
