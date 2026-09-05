package cli

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/broker"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/secret"
)

// daemon runs the resident process systemd starts: the background work and the API socket, never the lifecycle.
func (a App) daemon(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("daemon takes no arguments, got %d", len(args))
	}

	return daemon.New(a.Root, a.Out, apiTask{app: a}, proxyTask{app: a}).Run(ctx)
}

// apiTask serves the REST API on the socket under the root. The daemon restarts it when the listener dies.
type apiTask struct {
	app App
}

func (apiTask) Name() string { return "api" }

func (t apiTask) Run(ctx context.Context) error {
	repo, err := t.app.deps().repo()
	if err != nil {
		return err
	}

	listener, mode, group, err := api.Listen(t.app.Root)
	if err != nil {
		return err
	}

	owner := "no " + api.Group + " group on this host"
	if group != "" {
		owner = "group " + group
	}
	log.New(t.app.Out, "", log.LstdFlags).Printf("api listening on %s, mode %04o, %s", filepath.Join(t.app.Root, api.SocketFile), mode, owner)

	return api.Serve(ctx, listener, api.NewHandler(t.app.Version, repo, t.app.Out))
}

// proxyTask runs the egress proxy every fronted sandbox's web traffic is turned to, on the bridge gateway.
type proxyTask struct {
	app App
}

func (proxyTask) Name() string { return "proxy" }

func (t proxyTask) Run(ctx context.Context) error {
	d := t.app.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	// The proxy is the one reader of a value, so it takes the real store and not the seam the verbs drive.
	secrets, err := secret.New(filepath.Join(t.app.Root, "secrets"), d.holders)
	if err != nil {
		return err
	}
	d.secretSvc = secrets

	source, err := d.egress()
	if err != nil {
		return err
	}

	// Nothing can listen on the gateway before the bridge carries it, so the host side is built first.
	hostNet, err := d.net()
	if err != nil {
		return err
	}
	if err := hostNet.ReapplyAll(ctx); err != nil {
		return err
	}

	gateway, err := network.Gateway(network.Config{Root: t.app.Root})
	if err != nil {
		return err
	}

	ca, err := proxy.LoadCA(filepath.Join(t.app.Root, "proxy"))
	if err != nil {
		return err
	}

	logger := log.New(t.app.Out, "", log.LstdFlags)
	server, err := proxy.New(proxy.Config{Address: gateway, CA: ca, Director: broker.New(repo, source, secrets), Log: logger})
	if err != nil {
		return err
	}

	logger.Printf("proxy listening on %s, plain %d and tls %d", gateway, proxy.PlainPort, proxy.TLSPort)

	return server.Run(ctx)
}
