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
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// checkpointFile is the one file every snapshot holds, and the provider writes it last before it deletes.
const checkpointFile = "checkpoint.img"

// CopyRequest names the sandbox a fork or a clone makes. It is the JSON body of both routes.
type CopyRequest struct {
	Name string `json:"name,omitempty"`
}

// Pause writes a running sandbox into its snapshot directory and frees its memory. The record keeps
// the address, the writable layer stays on disk, and resume brings the whole thing back from those.
func (s *Service) Pause(ctx context.Context, ref string) (models.Sandbox, error) {
	if err := requireVerb(s.cfg.Provider, models.VerbPause); err != nil {
		return models.Sandbox{}, err
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	// A stop or a second pause would end or delete the sandbox this one is about to snapshot.
	unlock := s.lock(id)
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if sb.State != models.StateRunning {
		return models.Sandbox{}, &StateError{ID: id, State: sb.State, Fix: "pause takes a running sandbox"}
	}

	dir, err := s.cfg.Repo.SnapshotDir(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := s.cfg.Provider.Pause(ctx, id, dir); err != nil {
		return models.Sandbox{}, errors.Join(err, s.reconcileGone(ctx, id, dir))
	}

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StatePaused
		sb.PID = 0
		sb.Snapshot = dir

		return nil
	})
	if err != nil {
		return models.Sandbox{}, fmt.Errorf("sandbox %s is paused but its record was not updated: %w", id, err)
	}

	return s.record(id)
}

// reconcileGone is for a pause that failed: the sandbox still runs and the record is right, or the
// snapshot is complete and only the host cleanup failed, or the substrate lost it on the way.
func (s *Service) reconcileGone(ctx context.Context, id, dir string) error {
	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Alive() {
		return nil
	}

	// The checkpoint is the last file the provider writes before it deletes, so its presence means paused.
	_, statErr := os.Stat(filepath.Join(dir, checkpointFile))
	state := models.StateStopped
	if statErr == nil {
		state = models.StatePaused
	}

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = state
		sb.PID = 0
		if state == models.StatePaused {
			sb.Snapshot = dir
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is gone but its record was not updated: %w", id, err)
	}

	if state == models.StatePaused {
		return fmt.Errorf("sandbox %s is paused, but the host cleanup after the snapshot failed", id)
	}

	return fmt.Errorf("sandbox %s is gone from %s and its record says stopped", id, s.cfg.Provider.Name())
}

// Resume runs a paused sandbox again from its snapshot. It is the run the pause froze, so the record
// keeps the exit its entrypoint may already have had, and the snapshot stays for the next resume.
func (s *Service) Resume(ctx context.Context, ref string) (models.Sandbox, error) {
	if err := requireVerb(s.cfg.Provider, models.VerbResume); err != nil {
		return models.Sandbox{}, err
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	// Two resumes of one sandbox would each restore it; the second waits and then sees it running.
	unlock := s.lock(id)
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if sb.State != models.StatePaused {
		return models.Sandbox{}, &StateError{ID: id, State: sb.State, Fix: "resume takes a paused sandbox"}
	}
	if sb.Snapshot == "" {
		return models.Sandbox{}, &StateError{ID: id, State: sb.State, Fix: "its record names no snapshot to resume from"}
	}

	// The lease survived the pause, so this hands back the same address over a namespace built again.
	if _, err := s.cfg.Network.Allocate(ctx, id); err != nil {
		return models.Sandbox{}, err
	}

	if err := s.cfg.Provider.Resume(ctx, id, sb.Snapshot); err != nil {
		return models.Sandbox{}, errors.Join(err, Reconcile(ctx, s.cfg.Repo, s.cfg.Provider, id, true))
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

// Fork starts a new sandbox from the snapshot of another, and reads nothing else of the source.
func (s *Service) Fork(ctx context.Context, ref string, req CopyRequest) (sb models.Sandbox, err error) {
	if err := requireVerb(s.cfg.Provider, models.VerbFork); err != nil {
		return models.Sandbox{}, err
	}

	source, src, unlock, err := s.readSource(ctx, ref, req)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	if src.Snapshot == "" {
		return models.Sandbox{}, &StateError{ID: source, State: src.State, Fix: "pause it first, fork reads what the pause wrote"}
	}

	var td Teardown

	// The memory image holds the source's run, so an entrypoint that had exited before the pause has too.
	claim, err := s.claimCopy(ctx, &td, req, models.Sandbox{
		Image:      src.Image,
		Resources:  src.Resources,
		Secrets:    slices.Clone(src.Secrets),
		Policy:     src.Policy,
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
	if err := s.cfg.Provider.Fork(ctx, src.Snapshot, spec); err != nil {
		// An interrupt kills the restore process, not what it may already have restored, and only stop
		// ends a sandbox, so an unknown outcome is kept.
		if ctx.Err() != nil {
			td.Discard()

			return models.Sandbox{}, fmt.Errorf("the fork into sandbox %s was interrupted, so it may be running and it stays on the host: %w", id, err)
		}

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

// Clone starts a new sandbox over a copy of the files another one kept, and runs its entrypoint from
// the beginning. It takes no memory: that is fork, which reads a snapshot.
func (s *Service) Clone(ctx context.Context, ref string, req CopyRequest) (sb models.Sandbox, err error) {
	// No capability gate: every provider copies files and starts a sandbox, so clone is mandatory.
	source, src, unlock, err := s.readSource(ctx, ref, req)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	if src.State != models.StateStopped && src.State != models.StatePaused {
		return models.Sandbox{}, &StateError{ID: source, State: src.State, Fix: "stop it first, clone copies what a stop kept"}
	}

	var td Teardown

	// The entrypoint runs from the beginning, so the source's exit is not the clone's.
	claim, err := s.claimCopy(ctx, &td, req, models.Sandbox{
		Image:     src.Image,
		Resources: src.Resources,
		Secrets:   slices.Clone(src.Secrets),
		Policy:    src.Policy,
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
	if err := s.cfg.Provider.Clone(ctx, source, spec); err != nil {
		// An interrupt kills the start process and not what it started, and only stop ends a sandbox.
		if ctx.Err() != nil {
			td.Discard()

			return models.Sandbox{}, fmt.Errorf("the clone into sandbox %s was interrupted, so it may have started and it stays on the host: run shard rm %s: %w", id, id, err)
		}

		return models.Sandbox{}, err
	}

	// The commit point: the clone is live, so nothing below gives anything back.
	td.Discard()

	if err := RecordRunning(ctx, s.cfg.Repo, s.cfg.Provider, id, false); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// readSource takes the source of a fork or a clone and holds it, so no verb changes it under the copy.
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

	unlock := s.lock(id)

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		unlock()

		return "", models.Sandbox{}, nil, err
	}

	return id, sb, unlock, nil
}

// copyClaim is the sandbox a fork or a clone brings up over, held until the verb that claimed it ends.
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
	claim.unlock = s.lock(claim.id)

	claim.dir, err = s.cfg.Repo.Dir(claim.id)
	if err != nil {
		return claim, err
	}

	td.Push(func(ctx context.Context) error { return s.cfg.Network.Release(ctx, claim.id) })

	claim.net, err = AllocateNetwork(ctx, s.cfg.Network, claim.id)
	if err != nil {
		return claim, err
	}

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

	// The chain is keyed by the address, which the record holds only now, so the host learns it before the guest runs.
	if from.Policy != "" {
		if err := s.cfg.Network.Reapply(ctx, claim.id); err != nil {
			return claim, err
		}
	}

	return claim, nil
}
