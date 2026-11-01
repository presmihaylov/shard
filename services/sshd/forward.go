package sshd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/splice"
)

// dialBudget is a host port forward's own bound on the reach into the sandbox.
const dialBudget = 15 * time.Second

// loopback is every name of the sandbox's own loopback a -L may target; nothing else is reachable through it.
var loopback = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

func (c *connection) openForward(ch ssh.NewChannel, wg *sync.WaitGroup) {
	var msg struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(ch.ExtraData(), &msg); err != nil {
		c.reject(ch, ssh.ConnectionFailed, "the forward names no destination shard can read")

		return
	}
	if !loopback[msg.Host] {
		c.reject(ch, ssh.Prohibited, fmt.Sprintf("shard forwards only to the sandbox's own loopback, not to %s: use localhost", msg.Host))

		return
	}
	if msg.Port == 0 || msg.Port > math.MaxUint16 {
		c.reject(ch, ssh.ConnectionFailed, fmt.Sprintf("%d is no TCP port", msg.Port))

		return
	}
	port := uint16(msg.Port)
	if !acquire(c.forwards) {
		c.reject(ch, ssh.ResourceShortage, fmt.Sprintf("this connection has %d forwards open already", maxForwards))

		return
	}

	wg.Go(func() {
		defer func() { <-c.forwards }()
		c.forward(ch, port)
	})
}

// forward dials before it accepts, so a port nothing listens on is the client's open failure, as with sshd.
func (c *connection) forward(ch ssh.NewChannel, port uint16) {
	ctx, cancel := context.WithTimeout(c.ctx, dialBudget)
	upstream, err := c.srv.sandboxes.DialPort(ctx, c.opts.Sandbox, port)
	cancel()
	if err != nil {
		c.reject(ch, ssh.ConnectionFailed, c.unreached(port, err))

		return
	}

	channel, reqs, err := ch.Accept()
	if err != nil {
		c.srv.report(fmt.Sprintf("ssh: sandbox %s: accept a forward to port %d: %v", c.opts.Sandbox, port, errors.Join(err, upstream.Close())))

		return
	}
	var discard sync.WaitGroup
	discard.Go(func() { ssh.DiscardRequests(reqs) })
	defer discard.Wait()

	if err := splice.Conns(channel, upstream); err != nil {
		c.srv.report(fmt.Sprintf("ssh: sandbox %s: a forward to port %d broke: %v", c.opts.Sandbox, port, err))
	}
}

// unreached says why a dial into the sandbox failed, as a host port forward words it.
func (c *connection) unreached(port uint16, err error) string {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf("nothing listens on 127.0.0.1:%d in the sandbox", port)
	}
	if errors.Is(err, models.ErrUnsupported) {
		return "the sandbox runs a shard-init from before port forwards: stop and start it"
	}

	return c.opts.Explain(err)
}
