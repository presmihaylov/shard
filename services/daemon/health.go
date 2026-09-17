package daemon

import (
	"context"
	"log"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
)

// healthCheck runs the probe every running record asks for, on its own interval, and logs each change of status.
type healthCheck struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

// A tick is what a probe can be late by at most, so it stays well under the shortest interval a record names.
const healthInterval = time.Second

func (healthCheck) Name() string { return "health-check" }

func (t healthCheck) Run(ctx context.Context) error {
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
		// A root with nothing to probe needs no substrate, so a host without runsc keeps its daemon.
		if !slices.ContainsFunc(sandboxes, probed) {
			continue
		}

		svc, err := t.lifecycle.service()
		if err != nil {
			return err
		}
		if err := svc.CheckHealth(ctx, sandboxes, time.Now().UTC(), func(line string) { logger.Print(line) }); err != nil {
			return err
		}
	}
}

func probed(sb models.Sandbox) bool {
	return sb.State == models.StateRunning && sb.HealthCheck != nil
}
