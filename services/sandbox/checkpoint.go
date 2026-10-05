package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// checkpointFile is the one file every checkpoint holds, and the provider writes it last before it deletes.
const checkpointFile = "checkpoint.img"

// CopyRequest names the sandbox a fork makes. It is the JSON body of the fork route.
type CopyRequest struct {
	Name string `json:"name,omitempty"`
}

// Pause frees a running sandbox's memory into its checkpoint; the record and the writable layer stay for resume.
func (s *Service) Pause(ctx context.Context, ref string) (models.Sandbox, error) {
	if err := requireVerb(s.cfg.Provider, models.VerbPause); err != nil {
		return models.Sandbox{}, err
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	// A stop or a second pause would end or delete the sandbox this one is about to checkpoint.
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	if sb.State != models.StateRunning {
		return models.Sandbox{}, wrongState(id, sb, "pause takes a running sandbox", models.CodeSandboxNotRunning)
	}

	dir, err := s.cfg.Repo.CheckpointDir(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	// A checkpoint an earlier pause left would pass for this one's, so it goes before the mark that trusts one (SHARD-366).
	if err := os.Remove(filepath.Join(dir, checkpointFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return models.Sandbox{}, fmt.Errorf("remove the old checkpoint of sandbox %s: %w", id, err)
	}
	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Pausing = true

		return nil
	})
	if err != nil {
		return models.Sandbox{}, fmt.Errorf("mark the pause of sandbox %s: %w", id, err)
	}

	// A client that hangs up mid-checkpoint would cut a save the guest does not survive (SHARD-336).
	base := context.WithoutCancel(ctx)
	pctx, cancel := context.WithTimeout(base, s.pauseBudget())
	defer cancel()

	err = s.cfg.Provider.Pause(pctx, id, dir)
	// The silent process already spent its probe bound, so the record takes the reason without a second one.
	if silent, ok := errors.AsType[*models.UnresponsiveError](err); ok {
		if err := s.recordUnresponsive(id, sb, silent.Reason, s.report); err != nil {
			return models.Sandbox{}, err
		}
		sb.State = models.StateUnresponsive
		sb.UnresponsiveReason = silent.Reason

		return models.Sandbox{}, wrongState(id, sb, "pause takes a running sandbox", models.CodeSandboxNotRunning)
	}
	if err != nil {
		var lost *models.LostError
		if errors.As(err, &lost) {
			return models.Sandbox{}, s.fail(base, id, err)
		}

		// A pause that spent its budget leaves pctx done, so the reconcile probes under a budget of its own.
		return models.Sandbox{}, errors.Join(err, s.reconcileGone(base, id, dir))
	}

	if err := s.recordPaused(id, dir); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// recordPaused writes a pause whose checkpoint is complete, whether the pause itself or a reconcile found it.
func (s *Service) recordPaused(id, dir string) error {
	err := s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StatePaused
		sb.PID = 0
		sb.Checkpoint = dir
		sb.Pausing = false
		sb.UnresponsiveReason = ""

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is paused but its record was not updated: %w", id, err)
	}

	return nil
}

// dropCheckpoint removes the checkpoint a stop leaves behind, so a stopped sandbox keeps none of its pause's memory or disk copy (SHARD-592).
func (s *Service) dropCheckpoint(id string) error {
	dir, err := s.cfg.Repo.CheckpointDir(id)
	if err != nil {
		return err
	}

	// A partial .tmp from a cut pause goes too, the way Delete takes both.
	for _, path := range []string{dir, dir + ".tmp"} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove the checkpoint %s of sandbox %s: %w", path, id, err)
		}
	}

	return nil
}

// reconcileGone settles a failed pause: still running, checkpointed with only the host cleanup failed, or lost by the substrate.
func (s *Service) reconcileGone(ctx context.Context, id, dir string) error {
	status, err := s.status(ctx, id, "pause")
	if err != nil {
		return err
	}
	if status.Alive() {
		return s.settleLivePause(ctx, id, status)
	}

	// The checkpoint is the last file the provider writes, and this pause removed any older one before it began.
	held, err := hasCheckpoint(dir)
	if err != nil {
		return err
	}
	if held {
		if err := s.recordPaused(id, dir); err != nil {
			return err
		}

		return fmt.Errorf("sandbox %s is paused, but the host cleanup after the checkpoint failed", id)
	}

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateStopped
		sb.PID = 0
		sb.Pausing = false

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is gone but its record was not updated: %w", id, err)
	}

	return fmt.Errorf("sandbox %s is gone from %s and its record says stopped", id, s.cfg.Provider.Name())
}

// settleLivePause is for a failed pause the substrate still holds: running, or frozen beside the checkpoint the pause installed.
func (s *Service) settleLivePause(ctx context.Context, id string, status models.Status) error {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	// A delete that failed after the swap leaves the sentry frozen past its checkpoint, and a running record would lose the pause (SHARD-366).
	dir, err := s.cutPause(ctx, sb, status)
	if err != nil {
		return err
	}
	if dir != "" {
		if err := s.recordPaused(id, dir); err != nil {
			return err
		}

		return fmt.Errorf("sandbox %s is paused, but the host cleanup after the checkpoint had to be run again", id)
	}

	held, err := s.markedCheckpoint(sb)
	if err != nil {
		return err
	}
	// A substrate that cannot release holds the guest frozen beside a complete checkpoint, and only the mark still names that pause.
	if held != "" && status.State == models.StatePaused {
		return fmt.Errorf("sandbox %s is frozen beside the checkpoint its pause installed, and %s cannot release it: the record keeps the pause mark", id, s.cfg.Provider.Name())
	}

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Pausing = false

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s still runs but its record was not updated: %w", id, err)
	}

	return nil
}

// Resume continues the run the pause froze, so the record keeps any exit its entrypoint had and the checkpoint stays.
func (s *Service) Resume(ctx context.Context, ref string) (models.Sandbox, error) {
	if err := requireVerb(s.cfg.Provider, models.VerbResume); err != nil {
		return models.Sandbox{}, err
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	// Two resumes of one sandbox would each restore it; the second waits and then sees it running.
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	if sb.State != models.StatePaused {
		return models.Sandbox{}, &StateError{ID: id, State: sb.State, Fix: "resume takes a paused sandbox", Code: models.CodeSandboxNotPaused}
	}
	if sb.Checkpoint == "" {
		return models.Sandbox{}, &StateError{ID: id, State: sb.State, Fix: "it has no saved state to resume; remove it and create another sandbox", Code: models.CodeNoCheckpoint}
	}

	// The lease survived the pause, so this hands back the same address over a namespace built again.
	if _, err := s.cfg.Network.Allocate(ctx, id); err != nil {
		return models.Sandbox{}, err
	}

	if err := s.cfg.Provider.Resume(ctx, id, sb.Checkpoint); err != nil {
		return models.Sandbox{}, imageGone(id, sb.Image, sb.Digest, "resume", errors.Join(err, Reconcile(ctx, s.cfg.Repo, s.cfg.Provider, id, true)))
	}

	// The restore brought the guest up over rules it has no memory of, so the host's go on again now.
	if err := s.cfg.Network.Reapply(ctx, id); err != nil {
		err = fmt.Errorf("sandbox %s is running and its network rules were not applied again, so stop it or resume it again: %w", id, err)

		return models.Sandbox{}, errors.Join(err, Reconcile(ctx, s.cfg.Repo, s.cfg.Provider, id, true))
	}

	if err := RecordRunning(ctx, s.cfg.Repo, s.cfg.Provider, id, true); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// Fork starts a new sandbox from a capture of another as it runs, and reads nothing else of the source.
func (s *Service) Fork(ctx context.Context, ref string, req CopyRequest) (sb models.Sandbox, err error) {
	if err := requireVerb(s.cfg.Provider, models.VerbFork); err != nil {
		return models.Sandbox{}, err
	}

	source, src, unlock, err := s.readSource(ctx, ref, req)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	// A fork captures the source as it runs now, never an older checkpoint of it, so only a running source is forked (SHARD-457).
	if src.State != models.StateRunning {
		return models.Sandbox{}, wrongState(source, src, "fork takes a running sandbox", models.CodeSandboxNotRunning)
	}

	var td Teardown

	// The capture holds the source's run, so an entrypoint that had exited before it has in the fork too.
	claim, err := s.claimCopy(ctx, &td, req, models.Sandbox{
		Image:      src.Image,
		Digest:     src.Digest,
		Resources:  src.Resources,
		Secrets:    slices.Clone(src.Secrets),
		Policy:     src.Policy,
		Command:    slices.Clone(src.Command),
		Restart:    src.Restart,
		ExitStatus: src.ExitStatus,
	})
	defer claim.unlock()

	// After the unlock defer, so the unwind runs first and no verb sees the half-built copy.
	defer func() {
		if err != nil {
			err = errors.Join(err, td.Unwind(ctx))
		}
	}()

	if err != nil {
		return models.Sandbox{}, err
	}
	id := claim.id

	td.Push(func(ctx context.Context) error { return s.cfg.Provider.Remove(ctx, id) })

	spec := models.SandboxSpec{ID: id, Name: req.Name, StateDir: claim.dir, Network: claim.net, Resources: src.Resources}
	if err := s.cfg.Provider.Fork(ctx, source, spec); err != nil {
		if ctx.Err() == nil {
			return models.Sandbox{}, imageGone(source, src.Image, src.Digest, "fork", err)
		}
		// An interrupt kills the restore process, not what it may already have restored, and only stop ends a sandbox, so a fork that may run is kept.
		probe, perr := s.status(context.WithoutCancel(ctx), id, "fork")
		if perr != nil || probe.Alive() {
			td.Discard()

			return models.Sandbox{}, fmt.Errorf("the fork into sandbox %s was interrupted, so it may be running and it stays on the host: %w", id, errors.Join(err, perr))
		}

		// The substrate holds no live copy, so nothing runs under the claims and they go back.
		return models.Sandbox{}, err
	}

	// The commit point: the fork is live, so nothing below gives anything back.
	td.Discard()

	// The restored guest holds no rules of its own, so the host's go on again now.
	if err := s.cfg.Network.Reapply(ctx, id); err != nil {
		return models.Sandbox{}, errors.Join(err, Reconcile(ctx, s.cfg.Repo, s.cfg.Provider, id, true))
	}

	if err := RecordRunning(ctx, s.cfg.Repo, s.cfg.Provider, id, true); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// readSource takes the source of a fork and holds it, so no verb changes it under the copy.
func (s *Service) readSource(ctx context.Context, ref string, req CopyRequest) (string, models.Sandbox, func(), error) {
	if req.Name != "" {
		if err := sandboxstate.ValidName(req.Name); err != nil {
			return "", models.Sandbox{}, nil, err
		}
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", models.Sandbox{}, nil, err
	}

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return "", models.Sandbox{}, nil, err
	}

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		unlock()

		return "", models.Sandbox{}, nil, err
	}

	if err := FailedGuard(id, sb); err != nil {
		unlock()

		return "", models.Sandbox{}, nil, err
	}

	return id, sb, unlock, nil
}

// copyClaim is the sandbox a fork brings up over, held until the verb that claimed it ends.
type copyClaim struct {
	id  string
	dir string
	net models.NetworkSpec
	// unlock is a no-op until the record exists, because until then no other verb can name the copy.
	unlock func()
}

// claimCopy takes the record, the state directory and the network of the sandbox a copy brings up.
func (s *Service) claimCopy(ctx context.Context, td *Teardown, req CopyRequest, from models.Sandbox) (copyClaim, error) {
	claim := copyClaim{unlock: func() {}}

	from.Name = req.Name
	from.Provider = s.cfg.Provider.Name()
	from.State = models.StateCreated
	from.CreatedAt = time.Now().UTC()

	copied, err := s.cfg.Repo.Create(from)
	if err != nil {
		return claim, err
	}
	claim.id = copied.ID

	td.Push(func(context.Context) error { return s.cfg.Repo.Delete(claim.id) })

	// The id exists now, so a stop or an rm can name it: they wait here until the copy is done.
	unlock, err := s.lock(ctx, claim.id)
	if err != nil {
		return claim, err
	}
	claim.unlock = unlock

	claim.dir, err = s.cfg.Repo.Dir(claim.id)
	if err != nil {
		return claim, err
	}

	td.Push(func(ctx context.Context) error { return s.cfg.Network.Release(ctx, claim.id) })

	claim.net, err = AllocateNetwork(ctx, s.cfg.Network, claim.id)
	if err != nil {
		return claim, err
	}
	claim.net = resolvedThrough(claim.net, from.Policy)

	// The network lands in the record before the restore, so a copy that fails after it can be given back.
	err = s.cfg.Repo.Update(claim.id, func(sb *models.Sandbox) error {
		sb.NetnsPath = claim.net.NetnsPath
		sb.Address = claim.net.Address
		sb.HostInterface = claim.net.HostInterface

		return nil
	})
	if err != nil {
		return claim, err
	}

	// The rules are keyed by the address, which the record holds only now, so the host learns it before the guest runs.
	if egress.Fronted(from) {
		if err := s.cfg.Network.Reapply(ctx, claim.id); err != nil {
			return claim, err
		}
	}

	return claim, nil
}
