package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/presmihaylov/shard/services/sandbox"
)

// restartPolicy copies what the supervisor counted onto each record that has a policy, and logs each start again.
type restartPolicy struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

// A tick is a file read per record, so the count lags the supervisor by a second at most.
const restartInterval = time.Second

func (restartPolicy) Name() string { return "restart-policy" }

func (t restartPolicy) Run(ctx context.Context) error {
	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		sandboxes, err := repo.List()
		if err != nil {
			return err
		}
		// A root with no policy on a running record needs no substrate, so a host without runsc keeps its daemon.
		if !slices.ContainsFunc(sandboxes, sandbox.UnderRestartPolicy) {
			continue
		}

		svc, err := t.lifecycle.service()
		if err != nil {
			return err
		}
		if err := svc.RecordRestarts(ctx, sandboxes, func(line string) { logger.Print(line) }); err != nil {
			return err
		}
	}
}
