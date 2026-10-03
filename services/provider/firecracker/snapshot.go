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
	// Jailed says the vmstate names each drive by its path in the jail, which every jailed restore has; one from before the jail names host paths (SHARD-306).
	Jailed bool `json:"jailed"`
}

// Pause writes the paused VM into dir and ends its vmm: the memory and the overlay are on disk, and the record stays for the resume.
func (p *Provider) Pause(ctx context.Context, id string, dir string) error {
	m, err := p.install(ctx, id, dir)
	if err != nil {
		return err
	}

	// The install left the snapshot it replaced at tmp, and the new one is in place, so a Ctrl-C from here on must not leave a paused VM behind.
	return errors.Join(os.RemoveAll(dir+".tmp"), p.end(context.WithoutCancel(ctx), m))
}

// install puts the paused VM's snapshot in dir and answers its vmm, still paused beside it.
func (p *Provider) install(ctx context.Context, id string, dir string) (*machine, error) {
	stateDir, r, err := p.open(id)
	if err != nil {
		return nil, err
	}
	m, err := p.lookup(ctx, id, stateDir, r)
	if err != nil {
		return nil, err
	}
	status := models.Status{State: models.StateStopped}
	if m != nil {
		status = m.status(p)
	}
	if status.State != models.StateRunning {
		return nil, fmt.Errorf("sandbox %s is %s on %s: pause takes a running sandbox%s", id, status.State, Name, because(status))
	}
	// Only a boot puts a newer shard-init in the guest, so a VM booted before the freeze landed keeps one that cannot hold its root (SHARD-409).
	if !m.freezesOverlay {
		return nil, fmt.Errorf("sandbox %s runs a shard-init that cannot freeze the guest, which pause needs on %s: restart the sandbox, then pause it", id, Name)
	}
	// A vmm spawned before the jail would write a snapshot that names host paths, which no jailed restore can open (SHARD-306).
	if m.jail == "" {
		return nil, fmt.Errorf("sandbox %s runs a vmm from before the jail, whose snapshot no restore on %s can load: restart the sandbox, then pause it", id, Name)
	}

	// The snapshot is staged beside dir and swapped in whole, so dir never holds half of one.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return nil, fmt.Errorf("clear the snapshot directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create the snapshot directory %s: %w", tmp, err)
	}
	info, err := m.client.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	// A pause cut after the vCPUs stopped left the VM paused, and this one carries on from there.
	if info.State != fcapi.StatePaused {
		// A guest process the snapshot held mid-run would draw from the saved crng key before a restore's reseed, so the guest is frozen first (SHARD-409).
		if err := m.freeze(ctx); err != nil {
			return nil, abandon(m, tmp, fmt.Errorf("sandbox %s: freeze the guest before the pause: %w", id, err))
		}
		if err := m.client.Pause(); err != nil {
			return nil, abandon(m, tmp, fmt.Errorf("pause sandbox %s: %w", id, err))
		}
	}
	if err := p.stageSnapshot(m, r, stateDir, tmp); err != nil {
		return nil, abandon(m, tmp, fmt.Errorf("sandbox %s: %w", id, err))
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		return nil, abandon(m, tmp, fmt.Errorf("install the snapshot of sandbox %s: %w", id, err))
	}

	return m, nil
}

// stageSnapshot writes the vmm's state and memory, a copy of the overlay and the metadata into tmp, and marks it complete; the vCPUs are stopped, so the overlay is still.
func (p *Provider) stageSnapshot(m *machine, r record, stateDir, tmp string) error {
	snap := filepath.Join(m.jail, jailSnap)
	if err := p.snapshotInto(m, r, snap, tmp); err != nil {
		return errors.Join(err, os.RemoveAll(snap))
	}
	// The copy shares the overlay's blocks or is refused: a fork that copied every byte is not what the verb promises.
	if err := bundle.Reflink(filepath.Join(stateDir, bundle.OverlayDiskFile), filepath.Join(tmp, bundle.OverlayDiskFile)); err != nil {
		return fmt.Errorf("copy the overlay: %w", err)
	}
	meta := snapshot{BaseDisk: r.BaseDisk, RootFS: r.RootFS, Resources: r.Resources, Run: r.Run, Jailed: true}
	if err := writeJSON(filepath.Join(tmp, snapshotFile), meta); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, checkpointFile), nil, snapshotFileMode); err != nil {
		return fmt.Errorf("mark the snapshot complete: %w", err)
	}

	return nil
}

// snapshotInto has the vmm write its state and memory into a directory of the jail, then moves both into tmp as root's.
func (p *Provider) snapshotInto(m *machine, r record, snap, tmp string) error {
	// A pause cut after the snapshot left its files, which the vmm refuses to write over.
	if err := os.RemoveAll(snap); err != nil {
		return fmt.Errorf("clear %s: %w", snap, err)
	}
	if err := os.Mkdir(snap, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", snap, err)
	}
	if err := p.chown(snap, r.UID, r.UID); err != nil {
		return fmt.Errorf("give %s to uid %d: %w", snap, r.UID, err)
	}
	if err := m.client.Snapshot(jailSnap+jailState, jailSnap+jailMemory); err != nil {
		return fmt.Errorf("snapshot the vm: %w", err)
	}
	// The vmm wrote both as its own uid and with its own umask; out of the jail they are root's, as every other snapshot file is.
	for _, name := range []string{snapshotState, memoryFile} {
		path := filepath.Join(tmp, name)
		if err := os.Rename(filepath.Join(snap, name), path); err != nil {
			return fmt.Errorf("move %s out of the jail: %w", name, err)
		}
		if err := p.chown(path, 0, 0); err != nil {
			return fmt.Errorf("give %s to root: %w", name, err)
		}
		if err := os.Chmod(path, snapshotFileMode); err != nil {
			return fmt.Errorf("tighten %s: %w", name, err)
		}
	}

	return nil
}

// abandon gives up a pause that could not complete: the VM and its guest run on and the staging directory goes.
func abandon(m *machine, tmp string, err error) error {
	return errors.Join(err, runAgain(m), os.RemoveAll(tmp))
}

// freeze holds the guest for the pause in flight, which a stream dialed again meanwhile leaves frozen.
func (m *machine) freeze(ctx context.Context) error {
	m.freezing.Lock()
	defer m.freezing.Unlock()
	m.pausing = true

	return m.control.Load().Freeze(ctx)
}

// runAgain resumes the VM if the pause got that far, then thaws the guest, which a paused VM could never answer.
func runAgain(m *machine) error {
	// A reconnect swaps and thaws under freezing too, so either this thaw lands on the stream it put in, or that reconnect thaws.
	m.freezing.Lock()
	defer m.freezing.Unlock()
	m.pausing = false

	info, err := m.client.State(context.Background())
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", m.id, err)
	}
	if info.State == fcapi.StatePaused {
		if err := m.client.Resume(); err != nil {
			return fmt.Errorf("resume sandbox %s: %w", m.id, err)
		}
	}
	// The thaw outlives the pause's caller: a guest left frozen runs nothing again.
	if err := m.control.Load().Thaw(context.Background()); err != nil {
		return fmt.Errorf("sandbox %s: thaw the guest: %w", m.id, err)
	}

	return nil
}

// Resume brings the sandbox back from the snapshot in dir, in a fresh vmm over its own copy of the snapshot's overlay; the snapshot stays for the next one.
func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	stateDir, r, err := p.open(id)
	if err != nil {
		return err
	}
	if err := p.lost(id); err != nil {
		return err
	}
	if _, err := readSnapshot(dir); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	m, err := p.lookup(ctx, id, stateDir, r)
	if err != nil {
		return err
	}
	if err := p.endLeftover(ctx, m); err != nil {
		return err
	}
	if err := bundle.ReplaceDisk(func() error { return restoreFiles(dir, stateDir) }); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	// The memory holds the address and the run, so the guest is told neither again, and a cut that leaves it paused needs no marker: lookup resumes it.
	_, err = p.restore(ctx, id, stateDir, r, dir, false)

	return err
}

// endLeftover ends the vmm a pause left after its snapshot: paused, the snapshot is the truth and the vmm goes; running, the sandbox moved past it and is refused.
func (p *Provider) endLeftover(ctx context.Context, m *machine) error {
	if m == nil {
		return nil
	}
	status := m.status(p)
	if status.State == models.StateUnresponsive {
		return fmt.Errorf("sandbox %s is %s on %s: resume takes a paused sandbox%s", m.id, status.State, Name, because(status))
	}
	if !status.Alive() {
		return p.release(ctx, m)
	}
	if m.alive() {
		return fmt.Errorf("sandbox %s is %s on %s: resume takes a paused sandbox", m.id, status.State, Name)
	}

	return p.end(ctx, m)
}

// restoreFiles puts the snapshot's overlay under the sandbox in place of its own, or the restored memory would meet a filesystem it never wrote.
func restoreFiles(dir, stateDir string) error {
	overlay := filepath.Join(stateDir, bundle.OverlayDiskFile)
	if err := os.Remove(overlay); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("drop the overlay: %w", err)
	}
	if err := bundle.Reflink(filepath.Join(dir, bundle.OverlayDiskFile), overlay); err != nil {
		return fmt.Errorf("restore the overlay: %w", err)
	}
	// A state directory from before the jail links an older snapshot's memory, which no vmm maps now.
	if err := os.Remove(filepath.Join(stateDir, memoryFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("drop the memory: %w", err)
	}

	return nil
}

// restore brings the snapshot in dir up in a fresh vmm in a jail of the sandbox's own, over the overlay restoreFiles put in its directory.
// foreign marks a fork, whose guest wakes on the source's address, so a cut before the readdress must not resume it (SHARD-321).
func (p *Provider) restore(ctx context.Context, id, stateDir string, r record, dir string, foreign bool) (*machine, error) {
	if err := p.bound(id, r.Resources); err != nil {
		return nil, fmt.Errorf("restore sandbox %s: %w", id, err)
	}
	if foreign {
		if err := os.WriteFile(filepath.Join(stateDir, restoringFile), nil, 0o600); err != nil {
			return nil, fmt.Errorf("mark the restore of sandbox %s in flight: %w", id, err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, reseedFile), nil, 0o600); err != nil {
		return nil, fmt.Errorf("mark sandbox %s for a reseed: %w", id, err)
	}
	done := p.spawn(id)
	defer done()
	jail, err := p.jail(id, stateDir, &r, dir)
	if err != nil {
		return nil, fmt.Errorf("restore sandbox %s: %w", id, err)
	}
	// The state names each drive by its path in the jail, so the load opens this sandbox's own overlay, a fork's included.
	snap := fcapi.Snapshot{State: jailState, Memory: jailMemory, Tap: r.Tap, Vsock: jailVsock, Socket: apiSocket, Console: filepath.Join(stateDir, consoleFile)}
	client, info, err := fcapi.Restore(ctx, jail, snap)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("restore sandbox %s: %w", id, err), removeJail(r.Jail))
	}
	m, err := p.up(ctx, id, stateDir, r.Jail, client, info)
	if err != nil {
		return nil, err
	}
	if err := m.reseed(ctx); err != nil {
		return nil, errors.Join(err, p.end(ctx, m))
	}

	// Only a running sandbox is ever paused, so what a snapshot brings back is running and Status says so.
	p.mu.Lock()
	m.started = true
	p.mu.Unlock()

	return m, nil
}

// AdoptStaging drops the snapshot staging a cut pause left: resume reads the committed dir, never dir+".tmp", so a leftover stage is dead weight (SHARD-404).
func (p *Provider) AdoptStaging(dir string) error {
	return os.RemoveAll(dir + ".tmp")
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
	from, to := filepath.Join(dir, bundle.OverlayDiskFile), filepath.Join(spec.StateDir, bundle.OverlayDiskFile)
	if err := bundle.AdmitCopy(from, to, func() error { return restoreFiles(dir, spec.StateDir) }); err != nil {
		return fmt.Errorf("sandbox %s on %s: %w", spec.ID, Name, err)
	}

	// The memory restores under its own bounds; the network, and the name the guest answers to, are what the fork changes.
	r := record{BaseDisk: snap.BaseDisk, RootFS: snap.RootFS, Resources: snap.Resources, Run: snap.Run}
	r.network(spec)
	if err := writeRecord(spec.StateDir, r); err != nil {
		return err
	}
	// The guest holds the source's address until the readdress, so the marker tells the next daemon to end a fork it finds paused, never resume it.
	m, err := p.restore(ctx, spec.ID, spec.StateDir, r, dir, true)
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(spec.StateDir, recordFile)))
	}
	// The restored guest still answers to the source's address and MAC, which the readdress replaces in place.
	if err := m.readdress(ctx, r); err != nil {
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
	// No migration: shard is pre-alpha, and a fresh pause writes one that loads.
	if !snap.Jailed {
		return snapshot{}, fmt.Errorf("the snapshot in %s was taken before the jail, and no restore on %s can open the host paths it names: start the sandbox from its stopped state and pause it again", dir, Name)
	}

	return snap, nil
}
