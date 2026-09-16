package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
)

// oomRestart starts a sandbox the host ended for its memory again when its record asks: the kill skips runsc's cleanup.
type oomRestart struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

const oomInterval = 5 * time.Second

func (oomRestart) Name() string { return "oom-restart" }

func (t oomRestart) Run(ctx context.Context) error {
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
		// A root with nothing running needs no substrate, so a host without runsc keeps its daemon.
		if !slices.ContainsFunc(sandboxes, func(sb models.Sandbox) bool { return sb.State == models.StateRunning }) {
			continue
		}

		svc, err := t.lifecycle.service()
		if err != nil {
			return err
		}
		if err := svc.RestartOOMKilled(ctx, sandboxes, time.Now().UTC(), func(line string) { logger.Print(line) }); err != nil {
			return err
		}
	}
}
