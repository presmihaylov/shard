package daemon

import (
	"context"
	"log"
	"time"
)

// hostFirewall puts back the accepts for the bridge that a reload of ufw or of iptables flushes.
type hostFirewall struct {
	deps     *deps
	interval time.Duration
}

const firewallInterval = 5 * time.Second

type firewallOpener interface {
	OpenFirewall(ctx context.Context) error
}

func (hostFirewall) Name() string { return "firewall" }

func (t hostFirewall) Run(ctx context.Context) error {
	hostNet, err := t.deps.net()
	if err != nil {
		return err
	}
	// A VM host has no bridge, so the host firewall never sees a sandbox.
	opener, ok := hostNet.(firewallOpener)
	if !ok {
		return nil
	}

	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)
	failures := sandboxErrors{logger: logger, task: t.Name()}

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		failures.tick(ctx, opener.OpenFirewall(ctx))
	}
}
