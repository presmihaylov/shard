package vzvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
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
	if r.Paused {
		return fmt.Errorf("sandbox %s is already paused on %s", id, Name)
	}
	m, err := p.lookup(ctx, id, stateDir, r)
	if err != nil {
		return err
	}
	if m == nil || !m.status(p).Alive() {
		return fmt.Errorf("sandbox %s is %s on %s, and only a live one can pause", id, models.StateStopped, Name)
	}

	// The old snapshot stays until the new one is complete, so a failed pause loses nothing a fork needs.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear the snapshot directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("create the snapshot directory %s: %w", tmp, err)
	}
	if _, err := m.client.Pause(); err != nil {
		return fmt.Errorf("pause sandbox %s: %w", id, err)
	}
	if _, err := m.client.Save(filepath.Join(tmp, snapshotState)); err != nil {
		// A VM that could not be saved runs on, so the pause is refused rather than half done.
		_, resumeErr := m.client.Resume()

		return errors.Join(fmt.Errorf("save sandbox %s: %w", id, err), resumeErr, os.RemoveAll(tmp))
	}
	if err := p.end(ctx, m); err != nil {
		return err
	}

	// The disk is copied once the VM is off it, so the save and the disk are one moment.
	if _, err := bundle.CloneFile(filepath.Join(stateDir, diskFile), filepath.Join(tmp, snapshotDiskFile)); err != nil {
		return fmt.Errorf("copy the disk of sandbox %s: %w", id, err)
	}
	snap := snapshot{MachineID: r.MachineID, RootFS: r.RootFS, Resources: r.Resources, Run: r.Run}
	if err := writeJSON(filepath.Join(tmp, snapshotFile), snap); err != nil {
		return err
	}
	// The marker is what the sandbox service takes as a complete snapshot after a restart of the daemon.
	if err := os.WriteFile(filepath.Join(tmp, checkpointFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark the snapshot complete: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear the snapshot directory %s: %w", dir, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("move the snapshot into place: %w", err)
	}

	r.Paused = true

	return writeRecord(stateDir, r)
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
	snap, err := readSnapshot(dir)
	if err != nil {
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
