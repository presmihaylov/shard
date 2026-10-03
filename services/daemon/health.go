package daemon

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// healthCheck runs the probe every running record asks for, on its own interval, and logs each change of status.
type healthCheck struct {
	deps      *deps
	lifecycle *lifecycle
	interval  time.Duration
}

// A tick is what a probe can be late by at most, so it stays well under the shortest interval a record names.
const healthInterval = time.Second

// probeResult is what one sandbox's probe returned, so the task knows the sandbox is free to probe again.
type probeResult struct {
	id  string
	err error
}

func (healthCheck) Name() string { return "health-check" }

func (t healthCheck) Run(ctx context.Context) error {
	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)
	failures := sandboxErrors{logger: logger, task: t.Name()}

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	// Each probe runs on its own, so a slow one holds back no other sandbox (SHARD-363); the task ends after every probe it started.
	var probes sync.WaitGroup
	defer probes.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	running := map[string]bool{}
	results := make(chan probeResult)

	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-results:
			delete(running, r.id)
			// SHARD-376 (shard's ruling): a sandbox's error is logged and the task goes on, so one sandbox cannot hold back the rest.
			failures.probe(ctx, r.id, r.err)

			continue
		case <-ticker.C:
		}

		sandboxes, err := repo.List()
		if err != nil {
			return err
		}
		failures.forget(sandboxes, probed)
		// A root with nothing to probe needs no substrate, so a host without runsc keeps its daemon.
		if !slices.ContainsFunc(sandboxes, probed) {
			continue
		}

		svc, err := t.lifecycle.service()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, sb := range sandboxes {
			// A sandbox whose last probe is still out waits for it, so it never has two at once.
			if running[sb.ID] || !sandbox.ProbeDue(sb, now) {
				continue
			}
			running[sb.ID] = true
			probes.Go(func() {
				err := svc.CheckHealth(ctx, sb, now, func(line string) { logger.Print(line) })
				select {
				case results <- probeResult{id: sb.ID, err: err}:
				case <-ctx.Done():
				}
			})
		}
	}
}

func probed(sb models.Sandbox) bool {
	return sb.State == models.StateRunning && sb.HealthCheck != nil
}
