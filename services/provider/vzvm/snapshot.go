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
	state := models.StateStopped
	if m != nil {
		state = m.status(p).State
	}
	if state != models.StateRunning {
		return fmt.Errorf("sandbox %s is %s on %s: pause takes a running sandbox", id, state, Name)
	}

	// Everything that can fail happens while the VM is only paused, so a failed pause resumes it and loses nothing.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear the snapshot directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("create the snapshot directory %s: %w", tmp, err)
	}
	info, err := m.client.State()
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	// A pause that crashed before its record left the VM paused, and this one carries on from there.
	if info.State != vz.StatePaused {
		if _, err := m.client.Pause(); err != nil {
			return fmt.Errorf("pause sandbox %s: %w", id, err)
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

	return p.end(ctx, m)
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
	client, _, err := vz.Adopt(filepath.Join(stateDir, socketFile))
	if absent(err) {
		return nil
	}
	if err != nil {
		return err
	}

	return p.end(ctx, &machine{id: id, dir: stateDir, client: client})
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

// abandon gives up a pause that could not complete: the VM runs on and the staging directory goes.
func abandon(m *machine, tmp string, err error) error {
	_, resumeErr := m.client.Resume()

	return errors.Join(err, resumeErr, os.RemoveAll(tmp))
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

	return nil
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
	if err := os.Remove(disk); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("drop the disk of sandbox %s: %w", id, err)
	}
	if _, err := bundle.CloneFile(filepath.Join(dir, snapshotDiskFile), disk); err != nil {
		return fmt.Errorf("restore the disk of sandbox %s: %w", id, err)
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

// Fork restores the save in dir as a new sandbox under the spec's id and address; the source is not touched.
func (p *Provider) Fork(ctx context.Context, dir string, spec models.SandboxSpec) error {
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
	if _, err := bundle.CloneFile(filepath.Join(dir, snapshotDiskFile), filepath.Join(spec.StateDir, diskFile)); err != nil {
		return fmt.Errorf("copy the snapshot disk for sandbox %s: %w", spec.ID, err)
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
	if err := m.readdress(r); err != nil {
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
