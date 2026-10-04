package vzvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
	m, err := p.lookup(ctx, id, stateDir, r)
	if err != nil {
		return err
	}
	status := models.Status{State: models.StateStopped}
	if m != nil {
		status = m.status(p)
	}
	if status.State == models.StateUnresponsive {
		return &models.UnresponsiveError{Sandbox: id, Provider: Name, Verb: models.VerbPause, Reason: status.Reason}
	}
	if status.State != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s on %s: pause takes a running sandbox", id, status.State, Name)
	}

	// Everything that can fail happens while the VM is only paused, so a failed pause resumes it and loses nothing.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear the snapshot directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("create the snapshot directory %s: %w", tmp, err)
	}
	info, err := m.client.State(ctx)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	// A failed pause whose resume failed too left the VM paused, and this one carries on from there; a restart resumes it before this (SHARD-375).
	if info.State != vz.StatePaused {
		// A clone boots from the disk alone, so the guest's root is flushed and frozen first, and no write lands between the two.
		if err := m.freeze(ctx); err != nil {
			return abandon(m, tmp, fmt.Errorf("sandbox %s: freeze the guest's root before the pause: %w", id, err))
		}
		if _, err := m.client.Pause(); err != nil {
			return abandon(m, tmp, fmt.Errorf("pause sandbox %s: %w", id, err))
		}
	}
	if err := stageSnapshot(m, r, stateDir, tmp); err != nil {
		return abandon(m, tmp, fmt.Errorf("sandbox %s: %w", id, err))
	}
	// The record says paused before the swap, so a crash between the two leaves a resume that installs the staged snapshot and ends the shim.
	r.Paused = true
	r.Pauses++
	if err := writeRecord(stateDir, r); err != nil {
		return abandon(m, tmp, err)
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		r.Paused = false
		r.Pauses--

		return abandon(m, tmp, errors.Join(fmt.Errorf("install the snapshot of sandbox %s: %w", id, err), writeRecord(stateDir, r)))
	}

	// The install left the snapshot it replaced at tmp, which this pause owns and drops.
	return errors.Join(os.RemoveAll(tmp), p.end(ctx, m))
}

// finishPause installs what a crashed pause staged, proves a snapshot is in place and ends the shim it left.
func (p *Provider) finishPause(ctx context.Context, id, stateDir, dir string) error {
	if err := installStaged(dir); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	if _, err := readSnapshot(dir); err != nil {
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

// stageSnapshot writes the save, the disk and the metadata into tmp and marks it complete; the VM is paused, so the disk is still.
func stageSnapshot(m *machine, r record, stateDir, tmp string) error {
	if _, err := m.client.Save(filepath.Join(tmp, snapshotState)); err != nil {
		return fmt.Errorf("save the vm: %w", err)
	}
	if _, err := bundle.CloneFile(filepath.Join(stateDir, diskFile), filepath.Join(tmp, snapshotDiskFile)); err != nil {
		return fmt.Errorf("copy the disk: %w", err)
	}
	snap := snapshot{MachineID: r.MachineID, Pause: r.Pauses + 1, RootFS: r.RootFS, Resources: r.Resources, Run: r.Run}
	if err := writeJSON(filepath.Join(tmp, snapshotFile), snap); err != nil {
		return err
	}
	// The marker is what the sandbox service takes as a complete snapshot after a restart of the daemon.
	if err := os.WriteFile(filepath.Join(tmp, checkpointFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark the snapshot complete: %w", err)
	}

	return nil
}

// abandon gives up a pause that could not complete: the VM and its root run on and the staging directory goes.
func abandon(m *machine, tmp string, err error) error {
	return errors.Join(err, runAgain(m), os.RemoveAll(tmp))
}

// freeze holds the guest's root for the pause in flight, which a stream dialed again meanwhile leaves frozen.
func (m *machine) freeze(ctx context.Context) error {
	m.freezing.Lock()
	defer m.freezing.Unlock()
	m.pausing.Store(true)

	return m.control.Load().Freeze(ctx, models.VerbPause)
}

// runAgain resumes the VM if the pause got that far, then thaws the root, which a paused guest could never answer.
func runAgain(m *machine) error {
	// A reconnect swaps and thaws under freezing too, so either this thaw lands on the stream it put in, or that reconnect thaws.
	m.freezing.Lock()
	defer m.freezing.Unlock()
	m.pausing.Store(false)
	control := m.control.Load()

	info, err := m.client.State(context.Background())
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", m.id, err)
	}
	if info.State == vz.StatePaused {
		if _, err := m.client.Resume(); err != nil {
			return fmt.Errorf("resume sandbox %s: %w", m.id, err)
		}
	}
	// The thaw outlives the pause's caller: a guest left frozen takes no write again.
	if err := control.Thaw(context.Background()); err != nil {
		return fmt.Errorf("sandbox %s: thaw the guest's root: %w", m.id, err)
	}

	return nil
}

// installStaged finishes a pause that crashed after its record: a staged snapshot newer than the one in dir goes in, an older one goes.
func installStaged(dir string) error {
	tmp := dir + ".tmp"
	staged, err := readSnapshot(tmp)
	if errors.Is(err, fs.ErrNotExist) {
		return os.RemoveAll(tmp)
	}
	if err != nil {
		return err
	}
	current, err := readSnapshot(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && current.Pause >= staged.Pause {
		return os.RemoveAll(tmp)
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		return fmt.Errorf("install the staged snapshot: %w", err)
	}

	// The install left the older snapshot at tmp, which no resume and no fork reads again.
	return os.RemoveAll(tmp)
}

// Resume restores the save in dir over the sandbox's own disk, which the snapshot's copy replaces first.
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
	snap, err := readSnapshot(dir)
	if err != nil {
		return err
	}
	// A shim still up under a paused record is what a pause that crashed before its end left, and the save is the truth.
	if err := p.endLeftover(ctx, id, stateDir); err != nil {
		return err
	}

	// The disk comes back to the moment of the save, or the restored memory would meet a filesystem it never wrote.
	disk := filepath.Join(stateDir, diskFile)
	if err := bundle.ReplaceDisk(func() error {
		if err := os.Remove(disk); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("drop the disk of sandbox %s: %w", id, err)
		}
		if _, err := bundle.CloneFile(filepath.Join(dir, snapshotDiskFile), disk); err != nil {
			return fmt.Errorf("restore the disk of sandbox %s: %w", id, err)
		}

		return nil
	}); err != nil {
		return err
	}

	r.MachineID = snap.MachineID
	m, err := p.boot(ctx, id, stateDir, r, filepath.Join(dir, snapshotState))
	if err != nil {
		return err
	}

	r.Paused = false
	if err := writeRecord(stateDir, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}

	return nil
}

// AdoptStaging keeps the snapshot staging a cut pause left: a resume finishes it through installStaged, so dropping it would discard a saved VM (SHARD-404).
func (p *Provider) AdoptStaging(string) error { return nil }

// Fork refuses by name: the fork of a paused source is gone, and the live fork of a running one comes with SHARD-463 (SHARD-457).
func (p *Provider) Fork(context.Context, string, models.SandboxSpec) error {
	return models.Unsupported(Name, models.VerbFork)
}

// forkSnapshot restores the save in dir as a new sandbox under the spec's id and address, and leaves the source as it was; SHARD-463 builds the live fork on it.
func (p *Provider) forkSnapshot(ctx context.Context, dir string, spec models.SandboxSpec) error {
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
	snap, err := readSnapshot(dir)
	if err != nil {
		return err
	}

	if err := clear(spec.StateDir); err != nil {
		return err
	}
	if err := cloneDisk(filepath.Join(dir, snapshotDiskFile), filepath.Join(spec.StateDir, diskFile)); err != nil {
		return fmt.Errorf("copy the snapshot disk for sandbox %s on %s: %w", spec.ID, Name, err)
	}

	// The saved memory restores under its own identifier and size only; the network, and the name the guest answers to, are what the fork changes.
	r := record{MachineID: snap.MachineID, RootFS: firstNonEmpty(spec.RootFS, snap.RootFS), Resources: snap.Resources, Run: snap.Run}
	r.network(spec)
	if err := writeRecord(spec.StateDir, r); err != nil {
		return err
	}

	m, err := p.boot(ctx, spec.ID, spec.StateDir, r, filepath.Join(dir, snapshotState))
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(spec.StateDir, recordFile)))
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}

	return nil
}

func readSnapshot(dir string) (snapshot, error) {
	if _, err := os.Stat(filepath.Join(dir, checkpointFile)); err != nil {
		return snapshot{}, fmt.Errorf("no complete snapshot in %s: %w", dir, err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, snapshotFile))
	if err != nil {
		return snapshot{}, fmt.Errorf("read the snapshot in %s: %w", dir, err)
	}

	var snap snapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		return snapshot{}, fmt.Errorf("decode the snapshot in %s: %w", dir, err)
	}

	return snap, nil
}
