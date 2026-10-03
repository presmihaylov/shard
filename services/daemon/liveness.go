package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// liveness makes each running record agree with the substrate every tick.
type liveness struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

const livenessInterval = 5 * time.Second

func (liveness) Name() string { return "liveness" }

func (t liveness) Run(ctx context.Context) error {
	repo, err := t.deps.repo()
	if err != nil {
		return err
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

		sandboxes, err := sandboxstate.ListReadable(repo, t.deps.unreadableLog())
		if err != nil {
			return err
		}
		// A root with nothing live needs no substrate, so a host without runsc keeps its daemon.
		if !slices.ContainsFunc(sandboxes, func(sb models.Sandbox) bool {
			return sb.State.Live()
		}) {
			failures.tick(ctx, nil)

			continue
		}

		svc, err := t.lifecycle.service()
		if err != nil {
			return err
		}
		// SHARD-376 (shard's ruling): a sandbox's error is logged and the task goes on, so one sandbox cannot hold back the rest.
		failures.tick(ctx, svc.Liveness(ctx, sandboxes, func(line string) { logger.Print(line) }))
	}
}
