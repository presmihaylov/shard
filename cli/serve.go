package cli

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/network"
)

// serve runs the daemon: a resident peer over the same stores and locks as every other verb. It owns
// only background work, never the sandbox lifecycle, and systemd starts it, never another verb.
func (a App) serve(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("serve takes no arguments, got %d", len(args))
	}

	cfg, gateway, err := network.Layout(network.Config{Root: a.Root})
	if err != nil {
		return err
	}

	return daemon.New(a.Root, a.Out, proxyTask{app: a, gateway: gateway, ports: cfg.Proxy}).Run(ctx)
}

// proxyTask keeps a proxy serving the root. While a one-shot proxy holds the lock each run reports
// it and the backoff retry is the takeover loop: the run after that proxy dies wins the lock. A
// crash of the daemon's own proxy ends the run, and the restart is the recovery.
type proxyTask struct {
	app     App
	gateway netip.Addr
	ports   network.ProxyPorts
}

func (t proxyTask) Name() string { return "proxy" }

func (t proxyTask) Run(ctx context.Context) error {
	// Fresh deps each run, so a restart never serves over state a crash left behind.
	return t.app.serveProxy(ctx, t.app.deps(), t.gateway, t.ports)
}
