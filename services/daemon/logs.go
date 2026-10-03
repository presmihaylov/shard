package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/logfile"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/supervisor"
)

// heldLogRotation bounds the logs a process outside the daemon appends to; the daemon rotates the logs it writes itself as it writes them.
type heldLogRotation struct {
	deps *deps
}

// heldLogInterval is how long a held log may grow past its bound before the next pass, so a short one.
const heldLogInterval = time.Second

func (heldLogRotation) Name() string { return "held-log-rotation" }

func (t heldLogRotation) Run(ctx context.Context) error {
	ticker := time.NewTicker(heldLogInterval)
	defer ticker.Stop()

	// The first pass runs at once, so a log that grew while the daemon was down is bounded on start.
	for {
		if err := t.pass(); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// pass bounds the held logs of every sandbox the root records.
func (t heldLogRotation) pass() error {
	repo, err := t.deps.repo()
	if err != nil {
		return err
	}
	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)
	sandboxes, err := sandboxstate.ListReadable(repo, logger.Printf)
	if err != nil {
		return err
	}
	// A root with no sandbox needs no substrate, so a host without runsc keeps its daemon.
	if len(sandboxes) == 0 {
		return nil
	}
	provider, err := t.deps.provider()
	if err != nil {
		return err
	}

	return truncateHeldLogs(provider, sandboxes, supervisor.MaxLog)
}

// heldLogs is the one provider verb the rotation needs.
type heldLogs interface {
	HeldLogs(id string) ([]string, error)
}

// truncateHeldLogs bounds every sandbox before it reports a failure, so one log it cannot bound leaves the others bounded.
func truncateHeldLogs(provider heldLogs, sandboxes []models.Sandbox, max int64) error {
	var errs []error
	for _, sb := range sandboxes {
		paths, err := provider.HeldLogs(sb.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("the held logs of sandbox %s: %w", sb.ID, err))
			continue
		}
		for _, path := range paths {
			if err := logfile.Truncate(path, max); err != nil {
				errs = append(errs, fmt.Errorf("bound the log of sandbox %s: %w", sb.ID, err))
			}
		}
	}

	return errors.Join(errs...)
}

// outputLogs is the verb of a substrate whose daemon writes the output log itself, as a VM's does.
type outputLogs interface {
	BoundOutputLog(id string, max int64) error
}

// boundOutputLogs bounds the output logs a daemon before the bound left past max; only the reconciler calls it, before a probe attaches a VM and opens one.
func boundOutputLogs(provider models.Provider, sandboxes []models.Sandbox, max int64) error {
	owner, ok := provider.(outputLogs)
	if !ok {
		return nil
	}

	var errs []error
	for _, sb := range sandboxes {
		if err := owner.BoundOutputLog(sb.ID, max); err != nil {
			errs = append(errs, fmt.Errorf("bound the output log of sandbox %s: %w", sb.ID, err))
		}
	}

	return errors.Join(errs...)
}
