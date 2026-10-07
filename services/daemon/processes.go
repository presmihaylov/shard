package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// processTick copies shard-init's process table onto each running record that has processes, and logs each start again and each end.
type processTick struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

// A tick is a table read per record, so ps lags shard-init by a second at most.
const processInterval = time.Second

func (processTick) Name() string { return "processes" }

func (t processTick) Run(ctx context.Context) error {
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
		// A root with no process in a running record needs no substrate, so a host without runsc keeps its daemon.
		if !slices.ContainsFunc(sandboxes, sandbox.Supervised) {
			failures.tick(ctx, nil)

			continue
		}

		svc, err := t.lifecycle.service()
		if err != nil {
			return err
		}
		// SHARD-376 (shard's ruling): a sandbox's error is logged and the task goes on, so one sandbox cannot hold back the rest.
		failures.tick(ctx, svc.RecordProcesses(ctx, sandboxes, func(line string) { logger.Print(line) }))
	}
}
