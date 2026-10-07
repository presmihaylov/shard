package vzvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
)

// Pause saves the VM into dir and stops it: the memory is on disk, the shim is gone, and the record says paused.
func (p *Provider) Pause(ctx context.Context, id string, dir string) error {
	if !p.cfg.SaveRestore {
		return models.Unsupported(Name, models.VerbPause)
	}
	stateDir, r, err := p.open(id)
	if err != nil {
		return err
	}
	// A retry after a crash between the record and the swap lands here, and finishes that pause instead of refusing.
	if r.Paused {
		return p.finishPause(ctx, id, stateDir, dir)
	}
	m, r, err := p.checkpointSource(ctx, id, models.VerbPause)
	if err != nil {
		return err
	}

	// Everything that can fail happens while the VM is only paused, so a failed pause resumes it and loses nothing.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear the checkpoint directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("create the checkpoint directory %s: %w", tmp, err)
	}
	info, err := m.client.State(ctx)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	// A failed pause whose resume failed too left the VM paused, and this one carries on from there; a restart resumes it before this (SHARD-375).
	if info.State != vz.StatePaused {
		// The disk is copied apart from the memory, so the guest's root is flushed and frozen first, and no write lands between the two.
		if err := m.freeze(ctx, models.VerbPause); err != nil {
			return p.abandon(m, tmp, fmt.Errorf("sandbox %s: freeze the guest's root before the pause: %w", id, err))
		}
		if _, err := m.client.Pause(); err != nil {
			return p.abandon(m, tmp, fmt.Errorf("pause sandbox %s: %w", id, err))
		}
	}
	if err := stageCheckpoint(m, r, models.VerbPause, stateDir, tmp); err != nil {
		return p.abandon(m, tmp, fmt.Errorf("sandbox %s: %w", id, err))
	}
	// The record says paused before the swap, so a crash between the two leaves a resume that installs the staged checkpoint and ends the shim.
	r.Paused = true
	r.Pauses++
	if err := writeRecord(stateDir, r); err != nil {
		return p.abandon(m, tmp, err)
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		r.Paused = false
		r.Pauses--

		return p.abandon(m, tmp, errors.Join(fmt.Errorf("install the checkpoint of sandbox %s: %w", id, err), writeRecord(stateDir, r)))
	}

	// The install left the checkpoint it replaced at tmp, which this pause owns and drops.
	return errors.Join(os.RemoveAll(tmp), p.end(ctx, m))
}

// checkpointSource answers the shim of a running sandbox a save for verb can be taken of, and refuses any other.
func (p *Provider) checkpointSource(ctx context.Context, id, verb string) (*machine, record, error) {
	stateDir, r, err := p.open(id)
	if err != nil {
		return nil, record{}, err
	}
	m, err := p.lookup(ctx, id, stateDir, r)
	if err != nil {
		return nil, record{}, err
	}
	status := models.Status{State: models.StateStopped}
	if m != nil {
		status = m.status(p)
	}
	if status.State == models.StateUnresponsive {
		return nil, record{}, &models.UnresponsiveError{Sandbox: id, Provider: Name, Verb: verb, Reason: status.Reason}
	}
	if status.State != models.StateRunning {
		return nil, record{}, fmt.Errorf("sandbox %s is %s on %s: %s takes a running sandbox%s", id, status.State, Name, verb, because(status))
	}

	return m, r, nil
}

// finishPause installs what a crashed pause staged, proves a checkpoint is in place and ends the shim it left.
func (p *Provider) finishPause(ctx context.Context, id, stateDir, dir string) error {
	if err := installStaged(dir); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	if _, err := readCheckpoint(dir); err != nil {
		return fmt.Errorf("sandbox %s is paused on %s: %w", id, Name, err)
	}

	return p.endLeftover(ctx, id, stateDir)
}

// endLeftover stops the shim a crashed pause left; its guest is suspended, so it is never attached, only cut by its socket.
func (p *Provider) endLeftover(ctx context.Context, id, stateDir string) error {
	p.mu.Lock()
	m, held := p.machines[id]
	p.mu.Unlock()
	if held {
		return p.end(ctx, m)
	}
	socket := filepath.Join(stateDir, socketFile)
	client, _, err := vz.Adopt(ctx, socket)
	// A full socket queue refuses the dial too, so a refused leftover is ended by the pid its attach recorded, and gone only once that is (SHARD-423).
	if refused(err) {
		client, err = vz.Open(socket), nil
	}
	if absent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	shim, err := p.readShim(stateDir)
	if err != nil {
		return err
	}

	return p.end(ctx, &machine{id: id, dir: stateDir, client: client, shim: shim})
}

// stageCheckpoint writes the save, the disk and the metadata into tmp and marks it complete; the VM is paused, so the disk is still.
func stageCheckpoint(m *machine, r record, verb, stateDir, tmp string) error {
	// A save may reset every vsock stream of the guest, so the run after it dials the control stream again should the thaw find it gone.
	m.resetBy = verb
	if _, err := m.client.Save(filepath.Join(tmp, checkpointState)); err != nil {
		return fmt.Errorf("save the vm: %w", err)
	}
	if _, err := bundle.CloneFile(filepath.Join(stateDir, diskFile), filepath.Join(tmp, checkpointDiskFile)); err != nil {
		return fmt.Errorf("copy the disk: %w", err)
	}
	snap := checkpoint{MachineID: r.MachineID, Pause: r.Pauses + 1, RootFS: r.RootFS, Roots: r.Roots, Resources: r.Resources, Run: r.Run}
	if err := writeJSON(filepath.Join(tmp, checkpointMeta), snap); err != nil {
		return err
	}
	// The marker is what the sandbox service takes as a complete checkpoint after a restart of the daemon.
	if err := os.WriteFile(filepath.Join(tmp, checkpointFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark the checkpoint complete: %w", err)
	}

	return nil
}

// abandon gives up a pause that could not complete: the VM and its root run on and the staging directory goes.
func (p *Provider) abandon(m *machine, tmp string, err error) error {
	return errors.Join(err, p.runAgain(m), os.RemoveAll(tmp))
}

// freeze holds the guest's root for the verb in flight, which a stream dialed again meanwhile leaves frozen.
func (m *machine) freeze(ctx context.Context, verb string) error {
	m.freezing.Lock()
	defer m.freezing.Unlock()
	m.admit.Lock()
	m.pausing.Store(true)
	m.holder.Store(&verb)
	m.admit.Unlock()

	return m.control.Load().Freeze(ctx, verb)
}

// runAgain resumes the VM if the verb got that far, then thaws the root, which a paused guest could never answer.
func (p *Provider) runAgain(m *machine) error {
	// A reconnect swaps and thaws under freezing too, so either this thaw lands on the stream it put in, or that reconnect thaws.
	m.freezing.Lock()
	defer m.freezing.Unlock()
	defer m.holder.Store(nil)
	m.pausing.Store(false)
	verb := m.resetBy
	m.resetBy = ""

	info, err := m.client.State(context.Background())
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", m.id, err)
	}
	if info.State == vz.StatePaused {
		if _, err := m.client.Resume(); err != nil {
			return fmt.Errorf("resume sandbox %s: %w", m.id, err)
		}
	}
	// The thaw outlives the verb's caller: a guest left frozen takes no write again.
	thawed := m.control.Load().Thaw(context.Background())
	if thawed == nil {
		return nil
	}
	if verb == "" {
		return fmt.Errorf("sandbox %s: thaw the guest's root: %w", m.id, thawed)
	}
	// A save that reset the stream fails the thaw at once, so the run dials control again and thaws over that.
	m.kickLogs()

	return errors.Join(m.cutExecs(verb), p.redial(m, verb))
}

// redial puts in a control stream dialed again after verb's save, whose replay has adopt thaw the guest; the caller holds freezing.
func (p *Provider) redial(m *machine, verb string) error {
	deadline := time.Now().Add(redialGrace)
	for {
		if !m.alive() {
			return fmt.Errorf("sandbox %s stays frozen after the %s: its shim no longer runs the VM", m.id, verb)
		}
		adopted, err := p.dialAgain(m, time.Until(deadline))
		if adopted {
			return err
		}
		if time.Now().After(deadline) {
			// The follower waits on the stream the reset killed, so it ends here and the follower dials on for its own grace.
			return errors.Join(fmt.Errorf("sandbox %s stays frozen after the %s: its guest took no new control stream within %s: %w", m.id, verb, redialGrace, err), closeControl(m.control.Load()))
		}
		time.Sleep(max(pollInterval, m.refusals.Note(err)))
	}
}

// installStaged finishes a pause that crashed after its record: a staged checkpoint newer than the one in dir goes in, an older one goes.
func installStaged(dir string) error {
	tmp := dir + ".tmp"
	staged, err := readCheckpoint(tmp)
	if errors.Is(err, fs.ErrNotExist) {
		return os.RemoveAll(tmp)
	}
	if err != nil {
		return err
	}
	current, err := readCheckpoint(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && current.Pause >= staged.Pause {
		return os.RemoveAll(tmp)
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		return fmt.Errorf("install the staged checkpoint: %w", err)
	}

	// The install left the older checkpoint at tmp, which no resume and no fork reads again.
	return os.RemoveAll(tmp)
}

// Resume restores the save in dir over the sandbox's own disk, which the checkpoint's copy replaces first.
func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	if !p.cfg.SaveRestore {
		return models.Unsupported(Name, models.VerbResume)
	}
	stateDir, r, err := p.open(id)
	if err != nil {
		return err
	}
	if !r.Paused {
		return fmt.Errorf("sandbox %s is not paused on %s", id, Name)
	}
	if err := installStaged(dir); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	snap, err := readCheckpoint(dir)
	if err != nil {
		return err
	}
	// A shim still up under a paused record is what a pause that crashed before its end left, and the save is the truth.
	if err := p.endLeftover(ctx, id, stateDir); err != nil {
		return err
	}

	// The disk comes back to the moment of the save, or the restored memory would meet a filesystem it never wrote.
	if err := bundle.ReplaceDisk(func() error { return restoreDisk(id, dir, filepath.Join(stateDir, diskFile)) }); err != nil {
		return err
	}

	r.MachineID = snap.MachineID
	m, err := p.boot(ctx, id, stateDir, r, filepath.Join(dir, checkpointState))
	if err != nil {
		return err
	}

	r.Paused = false
	if err := writeRecord(stateDir, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}

	return nil
}

// restoreDisk puts the checkpoint's disk under the sandbox in place of its own, staged then swapped so a failed clone (ENOSPC) leaves the live disk for a later resume (SHARD-589).
func restoreDisk(id, dir, disk string) error {
	staged := disk + ".restore"
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear the staged disk of sandbox %s: %w", id, err)
	}
	if _, err := bundle.CloneFile(filepath.Join(dir, checkpointDiskFile), staged); err != nil {
		return fmt.Errorf("restore the disk of sandbox %s: %w", id, err)
	}
	if err := os.Rename(staged, disk); err != nil {
		return errors.Join(fmt.Errorf("swap the disk of sandbox %s: %w", id, err), os.Remove(staged))
	}

	return nil
}

// AdoptStaging keeps the checkpoint staging a cut pause left: a resume finishes it through installStaged, so dropping it would discard a saved VM (SHARD-404).
func (p *Provider) AdoptStaging(string) error { return nil }

// Fork captures the running source into the new sandbox's directory, runs the source on, and restores the capture as the fork.
func (p *Provider) Fork(ctx context.Context, source string, spec models.SandboxSpec) error {
	if !p.cfg.SaveRestore {
		return models.Unsupported(Name, models.VerbFork)
	}
	// The restore would refuse a fork id that runs only after the source was frozen for nothing.
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}
	capture := filepath.Join(spec.StateDir, captureDir)
	// A cut fork's capture would refuse this one's save and disk clone.
	if err := os.RemoveAll(capture); err != nil {
		return fmt.Errorf("clear the capture directory %s: %w", capture, err)
	}
	src, _, err := p.open(source)
	if err != nil {
		return err
	}
	// The copy is admitted before the capture stops the source, so a fork that cannot fit costs the source nothing (SHARD-775).
	if err := bundle.ReserveCopy(filepath.Join(src, diskFile), filepath.Join(spec.StateDir, diskFile)); err != nil {
		return fmt.Errorf("sandbox %s on %s: %w", spec.ID, Name, err)
	}
	defer bundle.Release(spec.StateDir)
	if err := p.capture(ctx, source, capture); err != nil {
		return errors.Join(err, os.RemoveAll(capture))
	}

	// The fork's disk is a clone of the capture's and its memory is restored, so the capture is spent.
	return errors.Join(p.forkCheckpoint(ctx, capture, spec), os.RemoveAll(capture))
}

// capture stages the running source's save in dir, then runs the source on, whatever the capture came to.
func (p *Provider) capture(ctx context.Context, id, dir string) error {
	m, err := p.hold(ctx, id, dir)
	if m == nil {
		return err
	}
	if runErr := p.runAgain(m); runErr != nil {
		return errors.Join(err, runErr)
	}

	return err
}

// hold stops the source over a frozen root and stages its save in dir; a machine it answers must run again, error or not.
func (p *Provider) hold(ctx context.Context, id, dir string) (*machine, error) {
	m, r, err := p.checkpointSource(ctx, id, models.VerbFork)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the capture directory %s: %w", dir, err)
	}
	// A fork boots from the disk clone, so the guest's root is flushed and frozen first, and no write lands between the save and the clone.
	if err := m.freeze(ctx, models.VerbFork); err != nil {
		return m, fmt.Errorf("sandbox %s: freeze the guest's root before the capture: %w", id, err)
	}
	if _, err := m.client.Pause(); err != nil {
		return m, fmt.Errorf("pause sandbox %s for the capture: %w", id, err)
	}
	if err := stageCheckpoint(m, r, models.VerbFork, m.dir, dir); err != nil {
		return m, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return m, nil
}

// forkCheckpoint restores the save in dir as a new sandbox under the spec's id and address, and leaves the source as it was.
func (p *Provider) forkCheckpoint(ctx context.Context, dir string, spec models.SandboxSpec) error {
	if !p.cfg.SaveRestore {
		return models.Unsupported(Name, models.VerbFork)
	}

	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}
	snap, err := readCheckpoint(dir)
	if err != nil {
		return err
	}

	if err := clear(spec.StateDir); err != nil {
		return err
	}
	if err := cloneDisk(filepath.Join(dir, checkpointDiskFile), filepath.Join(spec.StateDir, diskFile)); err != nil {
		return fmt.Errorf("copy the checkpoint disk for sandbox %s on %s: %w", spec.ID, Name, err)
	}

	// The saved memory restores under its own identifier and size only; the network, and the name the guest answers to, are what the fork changes.
	r := record{MachineID: snap.MachineID, RootFS: firstNonEmpty(spec.RootFS, snap.RootFS), Roots: snap.Roots, Resources: snap.Resources, Run: snap.Run}
	r.network(spec)
	if err := writeRecord(spec.StateDir, r); err != nil {
		return err
	}

	m, err := p.boot(ctx, spec.ID, spec.StateDir, r, filepath.Join(dir, checkpointState))
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(spec.StateDir, recordFile)))
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.end(ctx, m), os.Remove(filepath.Join(spec.StateDir, recordFile)))
	}

	return nil
}

func readCheckpoint(dir string) (checkpoint, error) {
	if _, err := os.Stat(filepath.Join(dir, checkpointFile)); err != nil {
		return checkpoint{}, fmt.Errorf("no complete checkpoint in %s: %w", dir, err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, checkpointMeta))
	if err != nil {
		return checkpoint{}, fmt.Errorf("read the checkpoint in %s: %w", dir, err)
	}

	var snap checkpoint
	if err := json.Unmarshal(blob, &snap); err != nil {
		return checkpoint{}, fmt.Errorf("decode the checkpoint in %s: %w", dir, err)
	}

	return snap, nil
}
