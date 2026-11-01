package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sshd"
)

// sshHost is the daemon's one ssh server, which the API's upgrade and a link's port 22 share, so two first starts never make two host keys.
type sshHost struct {
	root      string
	sandboxes sshd.Sandboxes
	report    func(string)

	mu  sync.Mutex
	srv *sshd.Server
}

// server loads the host key on the first ask, once the root is mounted.
func (h *sshHost) server() (*sshd.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.srv != nil {
		return h.srv, nil
	}
	signer, err := sshd.LoadHostKey(h.root)
	if err != nil {
		return nil, err
	}
	h.srv = sshd.New(signer, h.sandboxes, h.report)

	return h.srv, nil
}

func (h *sshHost) Serve(ctx context.Context, conn net.Conn, opts sshd.Options) error {
	srv, err := h.server()
	if err != nil {
		return errors.Join(err, conn.Close())
	}

	return srv.Serve(ctx, conn, opts)
}

// linkRecords reads the repository on the first link followed, as every getter builds on the first ask.
type linkRecords struct {
	deps *deps
}

func (r linkRecords) Get(id string) (models.Sandbox, error) {
	repo, err := r.deps.repo()
	if err != nil {
		return models.Sandbox{}, err
	}

	return repo.Get(id)
}

func (r linkRecords) List() ([]models.Sandbox, error) {
	repo, err := r.deps.repo()
	if err != nil {
		return nil, err
	}

	return repo.List()
}

// linkChanges puts the host rules on first and then ends the flows a change took away, which it does even when the rules fail.
type linkChanges struct {
	net   sandbox.Network
	flows sandbox.Links
}

func (c linkChanges) Changed(ctx context.Context, id string) error {
	return errors.Join(c.net.Reapply(ctx, id), c.flows.Changed(ctx, id))
}

// linkTask serves the forwarder on the bridge gateway, where the host rules dnat every flow to a link address.
type linkTask struct {
	deps *deps
	up   *gate
}

func (linkTask) Name() string { return "links" }

func (t linkTask) Run(ctx context.Context) error {
	// Nothing can listen on the gateway before the bridge carries it, so the host side is built first.
	hostNet, err := t.deps.net()
	if err != nil {
		return err
	}
	if err := hostNet.ReapplyAll(ctx); err != nil {
		return err
	}

	front, err := t.deps.front()
	if err != nil {
		return err
	}
	l, err := front.ListenTCP(network.LinkPort)
	if err != nil {
		return fmt.Errorf("listen for links: %w", err)
	}

	t.deps.logger().Printf("links listening on %s, port %d", hostNet.Gateway(), network.LinkPort)
	t.up.open()

	return t.deps.links.Serve(ctx, l)
}
