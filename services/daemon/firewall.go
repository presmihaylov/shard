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
	up       *gate
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
		t.up.open()

		return nil
	}

	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)
	failures := sandboxErrors{logger: logger, task: t.Name()}

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	// The first pass runs at once, since the autostart waits on it before any sandbox starts.
	for {
		err := opener.OpenFirewall(ctx)
		failures.tick(ctx, err)
		if err == nil {
			t.up.open()
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
