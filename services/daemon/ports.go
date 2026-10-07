package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// hostPorts keeps the forward listeners on the records: it opens them after a restart of the daemon, retries a port the host refused, and closes those of a sandbox that died.
type hostPorts struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

// A refused host port is retried each tick, so one another process let go of listens a second later.
const portsInterval = time.Second

func (hostPorts) Name() string { return "ports" }

func (t hostPorts) Run(ctx context.Context) error {
	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)
	failures := sandboxErrors{logger: logger, task: t.Name()}

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		if err := t.sync(ctx, repo, &failures); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// sync answers only an error that ends the task; a sandbox's own goes to failures.
func (t hostPorts) sync(ctx context.Context, repo *sandboxstate.Repository, failures *sandboxErrors) error {
	sandboxes, err := sandboxstate.ListReadable(repo, t.deps.unreadableLog())
	if err != nil {
		return err
	}
	// A root with no forward on record needs no substrate, so a host without runsc keeps its daemon; a verb closes what it drops.
	if !slices.ContainsFunc(sandboxes, func(sb models.Sandbox) bool { return len(sb.Ports) != 0 }) {
		failures.tick(ctx, nil)

		return nil
	}

	svc, err := t.lifecycle.service()
	if err != nil {
		return err
	}
	// SHARD-376 (shard's ruling): a sandbox's error is logged and the task goes on, so one sandbox cannot hold back the rest.
	failures.tick(ctx, svc.SyncPorts(sandboxes))

	return nil
}
