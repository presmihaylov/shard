package cli

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
)

// daemon runs the resident process: a resident peer over the same stores and locks as every other verb. It owns
// only background work, never the sandbox lifecycle, and systemd starts it, never another verb.
func (a App) daemon(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("daemon takes no arguments, got %d", len(args))
	}

	cfg, gateway, err := network.Layout(network.Config{Root: a.Root})
	if err != nil {
		return err
	}

	tasks := []daemon.Task{
		socketTask{app: a},
		proxyTask{app: a, gateway: gateway, ports: cfg.Proxy},
		rotateTask{app: a},
	}

	return daemon.New(a.Root, a.Out, tasks...).Run(ctx)
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

// socketTask answers the API on the socket under the root. The handlers read the same stores every
// verb reads, so the daemon needs no provider and no network to answer.
type socketTask struct {
	app App
}

func (t socketTask) Name() string { return "socket" }

func (t socketTask) Run(ctx context.Context) error {
	d := t.app.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	source, err := d.egress()
	if err != nil {
		return err
	}

	path := filepath.Join(t.app.Root, api.SocketFile)
	l, mode, err := api.Listen(path)
	if err != nil {
		return err
	}

	if err := t.app.print(fmt.Sprintf("api listening on %s, %s", path, mode)); err != nil {
		return errors.Join(err, l.Close())
	}

	svc := &api.Service{Binary: t.app.Version, Records: repo, Egress: source}

	return api.Serve(ctx, l, svc.Handler())
}

// rotateEvery paces the rotation scan. A stat per sandbox is cheap, and the size bound is generous.
const rotateEvery = time.Minute

// rotateTask keeps every sandbox's egress log bounded while the daemon runs.
type rotateTask struct {
	app App
}

func (t rotateTask) Name() string { return "egress-log-rotation" }

func (t rotateTask) Run(ctx context.Context) error {
	repo, err := t.app.deps().repo()
	if err != nil {
		return err
	}
	events := egress.NewEvents(repo.Dir)

	ticker := time.NewTicker(rotateEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := rotateAll(repo, events); err != nil {
				return err
			}
		}
	}
}

func rotateAll(repo sandboxRepo, events *egress.Events) error {
	sandboxes, err := repo.List()
	if err != nil {
		return err
	}

	for _, sb := range sandboxes {
		if err := events.Rotate(sb.ID); err != nil {
			return err
		}
	}

	return nil
}
