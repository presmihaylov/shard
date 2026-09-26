package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// snapshot is snapshot.json: what the frozen memory ran as, which a fork's record takes over from the source's.
type snapshot struct {
	BaseDisk  string             `json:"base_disk"`
	RootFS    string             `json:"rootfs,omitempty"`
	Resources models.Resources   `json:"resources"`
	Run       supervisor.RunSpec `json:"run"`
}

// Pause writes the paused VM into dir and ends its vmm: the memory and the overlay are on disk, and the record stays for the resume.
func (p *Provider) Pause(ctx context.Context, id string, dir string) error {
	stateDir, r, err := p.open(id)
	if err != nil {
		return err
	}
	m, err := p.lookup(ctx, id, stateDir)
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

	// The old snapshot stays until the new one is complete, so a failed pause loses nothing a fork needs.
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
	// A pause cut after the vCPUs stopped left the VM paused, and this one carries on from there.
	if info.State != fcapi.StatePaused {
		if err := m.client.Pause(); err != nil {
			return fmt.Errorf("pause sandbox %s: %w", id, err)
		}
	}
	if err := stageSnapshot(m, r, stateDir, tmp); err != nil {
		return abandon(m, tmp, fmt.Errorf("sandbox %s: %w", id, err))
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		return abandon(m, tmp, fmt.Errorf("install the snapshot of sandbox %s: %w", id, err))
	}

	// The install left the snapshot it replaced at tmp, and the new one is in place, so a Ctrl-C from here on must not leave a paused VM behind.
	return errors.Join(os.RemoveAll(tmp), p.end(context.WithoutCancel(ctx), m))
}

// stageSnapshot writes the vmm's state and memory, a copy of the overlay and the metadata into tmp, and marks it complete; the vCPUs are stopped, so the overlay is still.
func stageSnapshot(m *machine, r record, stateDir, tmp string) error {
	if err := m.client.Snapshot(filepath.Join(tmp, snapshotState), filepath.Join(tmp, memoryFile)); err != nil {
		return fmt.Errorf("snapshot the vm: %w", err)
	}
	// The copy shares the overlay's blocks or is refused: a fork that copied every byte is not what the verb promises.
	if err := bundle.Reflink(filepath.Join(stateDir, bundle.OverlayDiskFile), filepath.Join(tmp, bundle.OverlayDiskFile)); err != nil {
		return fmt.Errorf("copy the overlay: %w", err)
	}
	snap := snapshot{BaseDisk: r.BaseDisk, RootFS: r.RootFS, Resources: r.Resources, Run: r.Run}
	if err := writeJSON(filepath.Join(tmp, snapshotFile), snap); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, checkpointFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark the snapshot complete: %w", err)
	}

	return nil
}

// abandon gives up a pause that could not complete: the VM runs on and the staging directory goes.
func abandon(m *machine, tmp string, err error) error {
	return errors.Join(err, m.client.Resume(), os.RemoveAll(tmp))
}

// Resume brings the sandbox back from the snapshot in dir, in a fresh vmm over its own copy of the snapshot's overlay; the snapshot stays for the next one.
func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	stateDir, r, err := p.open(id)
	if err != nil {
		return err
	}
	if _, err := readSnapshot(dir); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	m, err := p.lookup(ctx, id, stateDir)
	if err != nil {
		return err
	}
	if err := p.endLeftover(ctx, m); err != nil {
		return err
	}
	if err := restoreFiles(dir, stateDir); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	// The memory holds the address and the run, so the guest is told neither again.
	_, err = p.restore(ctx, id, stateDir, r, dir)

	return err
}

// endLeftover ends the vmm a pause left after its snapshot: paused, the snapshot is the truth and the vmm goes; running, the sandbox moved past it and is refused.
func (p *Provider) endLeftover(ctx context.Context, m *machine) error {
	if m == nil {
		return nil
	}
	status := m.status(p)
	if !status.Alive() {
		return p.release(ctx, m)
	}
	if m.alive() {
		return fmt.Errorf("sandbox %s is %s on %s: resume takes a paused sandbox", m.id, status.State, Name)
	}

	return p.end(ctx, m)
}

// restoreFiles puts the snapshot's overlay and memory under the sandbox in place of its own, or the restored memory would meet a filesystem it never wrote.
func restoreFiles(dir, stateDir string) error {
	overlay := filepath.Join(stateDir, bundle.OverlayDiskFile)
	if err := os.Remove(overlay); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("drop the overlay: %w", err)
	}
	if err := bundle.Reflink(filepath.Join(dir, bundle.OverlayDiskFile), overlay); err != nil {
		return fmt.Errorf("restore the overlay: %w", err)
	}
	memory := filepath.Join(stateDir, memoryFile)
	if err := os.Remove(memory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("drop the memory: %w", err)
	}
	// The vmm maps the memory private, so the link is the snapshot's own file, shared with every other restore of it.
	if err := os.Link(filepath.Join(dir, memoryFile), memory); err != nil {
		return fmt.Errorf("link the memory: %w", err)
	}

	return nil
}

// restore brings the snapshot in dir up in a fresh vmm under the sandbox's directory, over the overlay and the memory restoreFiles put there.
func (p *Provider) restore(ctx context.Context, id, stateDir string, r record, dir string) (*machine, error) {
	group, err := p.bound(id, r.Resources)
	if err != nil {
		return nil, fmt.Errorf("restore sandbox %s: %w", id, err)
	}
	snap := fcapi.Snapshot{
		State:  filepath.Join(dir, snapshotState),
		Memory: filepath.Join(stateDir, memoryFile),
		Tap:    r.Tap,
		// The state names the overlay of the sandbox it was taken from, which the load opens; the swap points the drive at this one's before anything runs.
		Drives:  []fcapi.Drive{{ID: overlayDrive, Path: filepath.Join(stateDir, bundle.OverlayDiskFile)}},
		Vsock:   filepath.Join(stateDir, vsockFile),
		Socket:  filepath.Join(stateDir, socketFile),
		Console: filepath.Join(stateDir, consoleFile),
		Cgroup:  group,
	}
	client, info, err := fcapi.Restore(ctx, p.cfg.Binary, snap)
	if err != nil {
		return nil, fmt.Errorf("restore sandbox %s: %w", id, err)
	}
	m, err := p.up(ctx, id, stateDir, client, info)
	if err != nil {
		return nil, err
	}

	// Only a running sandbox is ever paused, so what a snapshot brings back is running and Status says so.
	p.mu.Lock()
	m.started = true
	p.mu.Unlock()

	return m, nil
}

// Fork brings the snapshot in dir up as a new sandbox under the spec's id, over its own copy of the overlay, and gives the guest the spec's address; the source is not touched.
func (p *Provider) Fork(ctx context.Context, dir string, spec models.SandboxSpec) error {
	snap, err := readSnapshot(dir)
	if err != nil {
		return err
	}
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}
	if err := clear(spec.StateDir); err != nil {
		return err
	}
	if err := restoreFiles(dir, spec.StateDir); err != nil {
		return fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	// The memory restores under its own bounds; the network, and the name the guest answers to, are what the fork changes.
	r := record{BaseDisk: snap.BaseDisk, RootFS: snap.RootFS, Resources: snap.Resources, Run: snap.Run}
	r.network(spec)
	if err := writeRecord(spec.StateDir, r); err != nil {
		return err
	}
	m, err := p.restore(ctx, spec.ID, spec.StateDir, r, dir)
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(spec.StateDir, recordFile)))
	}
	// The restored guest still answers to the source's address and MAC, which the readdress replaces in place.
	if err := m.readdress(r); err != nil {
		return errors.Join(err, p.end(ctx, m), os.Remove(filepath.Join(spec.StateDir, recordFile)))
	}

	return nil
}

// readSnapshot reads what a complete snapshot holds; one without its marker is a pause that did not finish, or no snapshot at all.
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
