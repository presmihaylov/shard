package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/network"
)

// teardownBudget bounds the whole give-back after a failed claim, on a context the verb's own cannot cancel.
const teardownBudget = 30 * time.Second

// Teardown is what a failed create, fork or clone gives back, in the reverse of the order it was claimed.
type Teardown struct {
	steps []func(context.Context) error
}

func (t *Teardown) Push(step func(context.Context) error) { t.steps = append(t.steps, step) }

// Discard is the commit point: what is on the stack now belongs to a sandbox that is live.
func (t *Teardown) Discard() { t.steps = nil }

// Unwind runs the stack on a fresh bounded context, since the verb's own may be cancelled, and stops at the first failure.
func (t *Teardown) Unwind(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownBudget)
	defer cancel()

	for i, step := range slices.Backward(t.steps) {
		if err := step(ctx); err != nil {
			return fmt.Errorf("gave back %d of %d claims and stopped, the rest are left on the host: %w",
				len(t.steps)-1-i, len(t.steps), err)
		}
	}

	return nil
}

// AllocateNetwork names the way out, because nothing expires on its own: only an rm frees an address.
func AllocateNetwork(ctx context.Context, net Network, id string) (models.NetworkSpec, error) {
	spec, err := net.Allocate(ctx, id)
	if errors.Is(err, network.ErrNoFreeAddress) {
		return models.NetworkSpec{}, fmt.Errorf("%w: every sandbox holds one until it is removed, run shard ls --all and rm the ones you no longer need", err)
	}

	return spec, err
}

// Reconcile is for a failed start or resume: the substrate may hold a live sandbox anyway, and only stop ends one.
func Reconcile(ctx context.Context, repo Repository, provider models.Provider, id string, keepExit bool) error {
	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if !status.Alive() {
		return nil
	}

	if err := RecordRunning(ctx, repo, provider, id, keepExit); err != nil {
		return err
	}

	return fmt.Errorf("sandbox %s may be running and it stays on the host", id)
}

// RecordRunning writes the substrate's pid into the record; keepExit is for a resume, whose entrypoint may be gone.
func RecordRunning(ctx context.Context, repo Repository, provider models.Provider, id string, keepExit bool) error {
	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}

	err = repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning
		sb.PID = status.PID
		sb.StoppedReason = ""
		if !keepExit {
			// The old exit is what the previous run did, and this run has not ended.
			sb.ExitStatus = nil
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is running but its record was not updated: %w", id, err)
	}

	return nil
}
