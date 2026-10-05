package firecracker

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
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// checkpoint is checkpoint.json: what the frozen memory ran as, which a fork's record takes over from the source's.
type checkpoint struct {
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

	// The install left the checkpoint it replaced at tmp, and the new one is in place, so a Ctrl-C from here on must not leave a paused VM behind.
	return errors.Join(os.RemoveAll(dir+".tmp"), p.end(context.WithoutCancel(ctx), m))
}

// install puts the paused VM's checkpoint in dir and answers its vmm, still paused beside it.
func (p *Provider) install(ctx context.Context, id string, dir string) (*machine, error) {
	m, r, err := p.checkpointSource(ctx, id, models.VerbPause)
	if err != nil {
		return nil, err
	}
	stateDir := m.dir
	// A capture marker that outlived its fork would spare this pause's frozen guest from the SHARD-427 end, should the pause be cut after its install.
	if err := os.Remove(filepath.Join(stateDir, captureFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("sandbox %s: clear the capture marker: %w", id, err)
	}

	// The checkpoint is staged beside dir and swapped in whole, so dir never holds half of one.
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return nil, fmt.Errorf("clear the checkpoint directory %s: %w", tmp, err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("create the checkpoint directory %s: %w", tmp, err)
	}
	info, err := m.client.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	// A pause cut after the vCPUs stopped left the VM paused, and this one carries on from there.
	if info.State != fcapi.StatePaused {
		// A guest process the checkpoint held mid-run would draw from the saved crng key before a restore's reseed, so the guest is frozen first (SHARD-409).
		if err := m.freeze(ctx, models.VerbPause); err != nil {
			return nil, p.abandon(m, tmp, fmt.Errorf("sandbox %s: freeze the guest before the pause: %w", id, err))
		}
		if err := m.client.Pause(); err != nil {
			return nil, p.abandon(m, tmp, fmt.Errorf("pause sandbox %s: %w", id, err))
		}
	}
	if err := p.stageCheckpoint(m, r, models.VerbPause, stateDir, tmp); err != nil {
		return nil, p.abandon(m, tmp, fmt.Errorf("sandbox %s: %w", id, err))
	}
	if err := store.SwapDir(tmp, dir); err != nil {
		return nil, p.abandon(m, tmp, fmt.Errorf("install the checkpoint of sandbox %s: %w", id, err))
	}

	return m, nil
}

// checkpointSource answers the vmm of a running sandbox a checkpoint for verb can be taken of, and refuses any other.
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
	// Only a boot puts a newer shard-init in the guest, so a VM booted before the freeze landed keeps one that cannot hold its root (SHARD-409).
	if !m.freezesOverlay {
		return nil, record{}, fmt.Errorf("sandbox %s runs a shard-init that cannot freeze the guest, which %s needs on %s: restart the sandbox, then %s it", id, verb, Name, verb)
	}
	// The Diff over resident pages missed a swapped page; the dirty-page log does not, so a later ticket can drop this (SHARD-450, SHARD-458).
	if err := p.noSwap(id); err != nil {
		return nil, record{}, err
	}
	// A vmm spawned before the jail would write a snapshot that names host paths, which no jailed restore can open (SHARD-306).
	if m.jail == "" {
		return nil, record{}, fmt.Errorf("sandbox %s runs a vmm from before the jail, whose checkpoint no restore on %s can load: restart the sandbox, then %s it", id, Name, verb)
	}

	return m, r, nil
}

// stageCheckpoint writes the vmm's state and memory, a copy of the overlay and the metadata into tmp, and marks it complete; the vCPUs are stopped, so the overlay is still.
func (p *Provider) stageCheckpoint(m *machine, r record, verb, stateDir, tmp string) error {
	snap := filepath.Join(m.jail, jailSnap)
	if err := p.snapshotInto(m, r, verb, snap, tmp); err != nil {
		return errors.Join(err, os.RemoveAll(snap))
	}
	// The copy shares the overlay's blocks or is refused: a fork that copied every byte is not what the verb promises.
	if err := bundle.Reflink(filepath.Join(stateDir, bundle.OverlayDiskFile), filepath.Join(tmp, bundle.OverlayDiskFile)); err != nil {
		return fmt.Errorf("copy the overlay: %w", err)
	}
	meta := checkpoint{BaseDisk: r.BaseDisk, RootFS: r.RootFS, Resources: r.Resources, Run: r.Run, Jailed: true}
	if err := writeJSON(filepath.Join(tmp, checkpointMeta), meta); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, checkpointFile), nil, checkpointFileMode); err != nil {
		return fmt.Errorf("mark the checkpoint complete: %w", err)
	}

	return nil
}

// seedMemory gives a restored vmm's Diff a copy of the memory it loaded to merge into, so the snapshot is whole; a booted vmm's Diff is whole on its own, with holes where the guest wrote nothing (SHARD-450).
func (p *Provider) seedMemory(m *machine, r record, snap string) error {
	loaded := filepath.Join(m.jail, jailMemory)
	restored, err := exists(loaded)
	if err != nil {
		return fmt.Errorf("read the loaded memory: %w", err)
	}
	if !restored {
		return nil
	}
	// The copy is a file of its own, so the merge never writes the memory the vmm maps.
	seed := filepath.Join(snap, memoryFile)
	if err := bundle.Reflink(loaded, seed); err != nil {
		return fmt.Errorf("copy the loaded memory: %w", err)
	}
	if err := p.chown(seed, r.UID, r.UID); err != nil {
		return fmt.Errorf("give %s to uid %d: %w", seed, r.UID, err)
	}
	// The loaded memory is read-only to the vmm, and a clone may keep that mode.
	if err := os.Chmod(seed, 0o600); err != nil {
		return fmt.Errorf("let the vmm write %s: %w", seed, err)
	}

	return nil
}

// snapshotInto has the vmm write its state and memory for verb into a directory of the jail, then moves both into tmp as root's.
func (p *Provider) snapshotInto(m *machine, r record, verb, snap, tmp string) error {
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
	// A vmm this process did not boot or load may have a log a snapshot already cleared, so it takes a Full, which needs no seed.
	kind := fcapi.SnapshotFull
	if m.wholeLog {
		kind = fcapi.SnapshotDiff
		if err := p.seedMemory(m, r, snap); err != nil {
			return err
		}
	}
	// The memory lands beside disks other sandboxes were promised room for, so it is admitted like one (SHARD-562).
	err := bundle.AdmitMemory(filepath.Join(m.dir, bundle.OverlayDiskFile), bundle.MemoryBound(r.Resources), func() error {
		// Once shard attempts a create, it conservatively treats the next snapshot as Full.
		m.wholeLog = false
		// The create resets every vsock stream of the guest, so the run after it dials the control stream again, and no exec waits on a dead one.
		m.resetBy = verb
		if err := m.client.Snapshot(snapshotWithin(r.Resources.MemoryMiB), kind, jailSnap+jailState, jailSnap+jailMemory); err != nil {
			return errors.Join(fmt.Errorf("capture the vm: %w", err), m.cutExecs(verb))
		}

		return m.cutExecs(verb)
	})
	if err != nil {
		return err
	}
	// The vmm wrote both as its own uid and with its own umask; out of the jail they are root's, as every other checkpoint file is.
	for _, name := range []string{checkpointState, memoryFile} {
		path := filepath.Join(tmp, name)
		if err := os.Rename(filepath.Join(snap, name), path); err != nil {
			return fmt.Errorf("move %s out of the jail: %w", name, err)
		}
		if err := p.chown(path, 0, 0); err != nil {
			return fmt.Errorf("give %s to root: %w", name, err)
		}
		if err := os.Chmod(path, checkpointFileMode); err != nil {
			return fmt.Errorf("tighten %s: %w", name, err)
		}
	}

	return nil
}

// snapshotWithin bounds one snapshot create of a guest with memoryMiB of memory.
func snapshotWithin(memoryMiB int64) time.Duration {
	return snapshotFloor + time.Duration(memoryMiB/snapshotRate)*time.Second
}

// abandon gives up a pause that could not complete: the VM and its guest run on and the staging directory goes.
func (p *Provider) abandon(m *machine, tmp string, err error) error {
	return errors.Join(err, p.runAgain(m), os.RemoveAll(tmp))
}

// freeze holds the guest for the verb in flight, which a stream dialed again meanwhile leaves frozen.
func (m *machine) freeze(ctx context.Context, verb string) error {
	m.freezing.Lock()
	defer m.freezing.Unlock()
	m.pausing = true
	m.holder.Store(&verb)

	return m.control.Load().Freeze(ctx, verb)
}

// runAgain resumes the VM if the pause got that far, then thaws the guest, over a control stream dialed again once a snapshot create reset the old one.
func (p *Provider) runAgain(m *machine) error {
	// A reconnect swaps and thaws under freezing too, so either this thaw lands on the stream it put in, or that reconnect thaws.
	m.freezing.Lock()
	defer m.freezing.Unlock()
	defer m.holder.Store(nil)
	m.pausing = false
	verb := m.resetBy
	m.resetBy = ""

	info, err := m.client.State(context.Background())
	if err != nil {
		return p.letGo(m, fmt.Errorf("sandbox %s: %w", m.id, err))
	}
	if info.State == fcapi.StatePaused {
		if err := m.client.Resume(); err != nil {
			return p.letGo(m, fmt.Errorf("resume sandbox %s: %w", m.id, err))
		}
	}
	if verb == "" {
		// The thaw outlives the pause's caller: a guest left frozen runs nothing again.
		if err := m.control.Load().Thaw(context.Background()); err != nil {
			return fmt.Errorf("sandbox %s: thaw the guest: %w", m.id, err)
		}

		return nil
	}
	m.kickLogs()

	return p.redial(m, verb)
}

// letGo drops a machine whose VM may still be paused, so the next lookup adopts it as a new daemon would, and resumes a VM no checkpoint stands for (SHARD-560).
func (p *Provider) letGo(m *machine, err error) error {
	// A vmm gone is the follower's to settle.
	if absent(err) {
		return err
	}
	p.forget(m)

	return errors.Join(err, m.close())
}

// redial puts in a control stream dialed again after verb's snapshot create, whose replay has adopt thaw the guest; the caller holds freezing.
func (p *Provider) redial(m *machine, verb string) error {
	deadline := time.Now().Add(redialGrace)
	for {
		if !m.alive() {
			return fmt.Errorf("sandbox %s stays frozen after the %s: its vmm no longer runs the VM", m.id, verb)
		}
		adopted, err := p.dialAgain(m, time.Until(deadline))
		if adopted {
			return err
		}
		if time.Now().After(deadline) {
			// The follower waits on the stream the reset killed, so it ends here and the follower dials on until a guest answers.
			return errors.Join(fmt.Errorf("sandbox %s stays frozen after the %s: its guest took no new control stream within %s: %w", m.id, verb, redialGrace, err), closeControl(m.control.Load()))
		}
		time.Sleep(max(pollInterval, m.refusals.Note(err)))
	}
}

// Resume brings the sandbox back from the checkpoint in dir, in a fresh vmm over its own copy of the checkpoint's overlay; the checkpoint stays for the next one.
func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	stateDir, r, err := p.open(id)
	if err != nil {
		return err
	}
	if err := p.lost(id); err != nil {
		return err
	}
	if _, err := readCheckpoint(dir); err != nil {
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

// endLeftover ends the vmm a pause left after its checkpoint: paused, the checkpoint is the truth and the vmm goes; running, the sandbox moved past it and is refused.
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

// restoreFiles puts the checkpoint's overlay under the sandbox in place of its own, or the restored memory would meet a filesystem it never wrote.
func restoreFiles(dir, stateDir string) error {
	overlay := filepath.Join(stateDir, bundle.OverlayDiskFile)
	// Stage the copy, then swap, so a failed clone (ENOSPC) leaves the live overlay in place and a later resume can retry (SHARD-589).
	staged := overlay + ".restore"
	if err := os.Remove(staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear the staged overlay: %w", err)
	}
	if err := bundle.Reflink(filepath.Join(dir, bundle.OverlayDiskFile), staged); err != nil {
		return fmt.Errorf("restore the overlay: %w", err)
	}
	if err := os.Rename(staged, overlay); err != nil {
		return errors.Join(fmt.Errorf("swap the overlay: %w", err), os.Remove(staged))
	}
	// A state directory from before the jail links an older checkpoint's memory, which no vmm maps now.
	if err := os.Remove(filepath.Join(stateDir, memoryFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("drop the memory: %w", err)
	}

	return nil
}

// restore boots the checkpoint in dir in a fresh jail; foreign marks a fork, which a cut before its readdress must not resume (SHARD-321).
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

	// Only a running sandbox is ever paused, so what a checkpoint brings back is running and Status says so.
	p.mu.Lock()
	m.started = true
	p.mu.Unlock()

	return m, nil
}

// AdoptStaging drops the checkpoint staging a cut pause left: resume reads the committed dir, never dir+".tmp", so a leftover stage is dead weight (SHARD-404).
func (p *Provider) AdoptStaging(dir string) error {
	return os.RemoveAll(dir + ".tmp")
}

// Fork captures the running source into the new sandbox's directory, runs the source on, and brings the capture up as the fork.
func (p *Provider) Fork(ctx context.Context, source string, spec models.SandboxSpec) error {
	capture := filepath.Join(spec.StateDir, captureDir)
	if err := p.capture(ctx, source, capture); err != nil {
		return errors.Join(err, os.RemoveAll(capture))
	}

	// The fork's jail took its own copies of the state and the memory, and its overlay is a copy too, so the capture is spent.
	return errors.Join(p.forkCheckpoint(ctx, capture, spec), os.RemoveAll(capture))
}

// capture stages the running source's checkpoint in dir, then runs the source on, whatever the capture came to.
func (p *Provider) capture(ctx context.Context, id, dir string) error {
	m, err := p.hold(ctx, id, dir)
	if m == nil {
		return err
	}

	// A source this run left paused keeps its marker, so the next daemon's adopt runs it again rather than end it as a cut pause.
	if runErr := p.runAgain(m); runErr != nil {
		return errors.Join(err, runErr)
	}

	return errors.Join(err, os.Remove(filepath.Join(m.dir, captureFile)))
}

// hold marks the source, stops it over a frozen guest and stages its checkpoint in dir; a machine it answers must run again, error or not.
func (p *Provider) hold(ctx context.Context, id, dir string) (*machine, error) {
	m, r, err := p.checkpointSource(ctx, id, models.VerbFork)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the capture directory %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, captureFile), nil, 0o600); err != nil {
		return nil, fmt.Errorf("mark the capture of sandbox %s: %w", id, err)
	}
	// A guest process the capture held mid-run would draw from the saved crng key before the fork's reseed, so the guest is frozen first (SHARD-409).
	if err := m.freeze(ctx, models.VerbFork); err != nil {
		return m, fmt.Errorf("sandbox %s: freeze the guest before the capture: %w", id, err)
	}
	if err := m.client.Pause(); err != nil {
		return m, fmt.Errorf("pause sandbox %s for the capture: %w", id, err)
	}
	if err := p.stageCheckpoint(m, r, models.VerbFork, m.dir, dir); err != nil {
		return m, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return m, nil
}

// forkCheckpoint brings the checkpoint in dir up as a new sandbox under the spec's id, over its own copy of the overlay, and gives the guest the spec's address.
func (p *Provider) forkCheckpoint(ctx context.Context, dir string, spec models.SandboxSpec) error {
	snap, err := readCheckpoint(dir)
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

// readCheckpoint reads what a complete checkpoint holds; one without its marker is a pause that did not finish, or no checkpoint at all.
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
	// No migration: shard is pre-alpha, and a fresh pause writes one that loads.
	if !snap.Jailed {
		return checkpoint{}, fmt.Errorf("the checkpoint in %s was taken before the jail, and no restore on %s can open the host paths it names: start the sandbox from its stopped state and pause it again", dir, Name)
	}

	return snap, nil
}
