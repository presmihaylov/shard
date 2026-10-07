package vzvm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Create clones the image disk, records what the VM boots with and boots it; nothing in the guest runs yet.
func (p *Provider) Create(ctx context.Context, spec models.SandboxSpec) error {
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}
	if spec.RootDisk == "" {
		return fmt.Errorf("sandbox %s: the image has no root disk, and a vm boots from one", spec.ID)
	}
	if err := checkMemory(spec); err != nil {
		return err
	}

	if err := clear(spec.StateDir); err != nil {
		return err
	}
	if err := writeDisk(spec); err != nil {
		return fmt.Errorf("sandbox %s on %s: %w", spec.ID, Name, err)
	}

	r, err := recordOf(spec)
	if err != nil {
		return err
	}

	return p.launch(ctx, spec, r)
}

// writeDisk clones the image's root disk, or the seed's disk, grown to the bound.
func writeDisk(spec models.SandboxSpec) error {
	to := filepath.Join(spec.StateDir, diskFile)
	if spec.Seed == "" {
		if err := bundle.CheckImage(spec.RootDisk); err != nil {
			return err
		}
		_, err := bundle.CloneRootDisk(spec.RootDisk, to, spec.Resources)

		return err
	}

	from := filepath.Join(spec.Seed, diskFile)

	return bundle.GrowSeed(to, spec.Resources, func() error {
		_, err := bundle.CloneFile(from, to)

		return err
	})
}

// launch records the sandbox, boots its VM, addresses the guest and reads a seed's own files.
func (p *Provider) launch(ctx context.Context, spec models.SandboxSpec, r record) error {
	id, dir := spec.ID, spec.StateDir
	if err := writeRecord(dir, r); err != nil {
		return err
	}

	m, err := p.boot(ctx, id, dir, r, "")
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(dir, recordFile)))
	}

	// The shim made the identifier on this first boot, and every later boot of this disk must reuse it.
	r.MachineID = m.machineID
	if err := writeRecord(dir, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}
	if spec.Seed != "" {
		r, err = seeded(r, spec, supervisor.GuestFiles(ctx, guestRun(m), bundle.MaxGuestFile))
		if err != nil {
			return errors.Join(err, p.end(ctx, m))
		}
		if err := writeRecord(dir, r); err != nil {
			return errors.Join(err, p.end(ctx, m))
		}
	}

	return nil
}

// guestRun runs one command as root in the VM a launch holds, before any exec can reach the sandbox.
func guestRun(m *machine) supervisor.ExecFunc {
	return func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		return supervisor.Exec(ctx, m.dial, m.id, supervisor.ExecHeader{Argv: spec.Argv, WorkDir: spec.WorkDir}, spec)
	}
}

// seeded resolves the user and the CA roots against the seed's own files, which only the booted guest can read off its disk (SHARD-784).
func seeded(r record, spec models.SandboxSpec, read bundle.ReadGuest) (record, error) {
	tree, err := bundle.GuestTree(bundle.GuestPaths(r.Run.Env, spec.User), read)
	if err != nil {
		return record{}, fmt.Errorf("sandbox %s: read the files of the seed %s: %w", spec.ID, spec.Seed, err)
	}
	if err := r.seed(tree, spec); err != nil {
		return record{}, errors.Join(fmt.Errorf("sandbox %s: %w", spec.ID, err), os.RemoveAll(tree))
	}

	return r, os.RemoveAll(tree)
}

// seed keeps the seed's roots for every later trust, then resolves against its files.
func (r *record) seed(tree string, spec models.SandboxSpec) error {
	roots, err := bundle.ReadRoots(tree, r.Run.Env)
	if err != nil {
		return err
	}
	r.Roots = &roots

	return r.identify(tree, spec.User, spec.ProxyCA)
}

// clear drops what an earlier run of this state directory left, so nothing of it answers for the new one.
func clear(dir string) error {
	for _, stale := range []string{oomFile, supervisorFailedFile, supervisor.ProcessTable, recordFile, diskFile, shimFile} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, supervisor.ProcessLogs)); err != nil {
		return fmt.Errorf("clear the process logs: %w", err)
	}

	return nil
}

// checkMemory refuses a bound the guest cannot boot under; zero is unbounded on Linux, and a VM has no such thing.
func checkMemory(spec models.SandboxSpec) error {
	if err := checkResources(spec.Resources); err != nil {
		return fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	return nil
}

func checkResources(res models.Resources) error {
	if res.MemoryMiB == 0 {
		return fmt.Errorf("provider %s needs resources.memory_mib, as a VM's memory is real memory on the host; set it to %d MiB or more", Name, MinMemoryMiB)
	}
	if res.MemoryMiB < MinMemoryMiB {
		return fmt.Errorf("resources.memory_mib is %d MiB, under the %d MiB provider %s needs; set it to %d MiB or more", res.MemoryMiB, MinMemoryMiB, Name, MinMemoryMiB)
	}
	if err := bundle.CheckGrowBound(bundle.DiskBound(res)); err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}

	return nil
}

// recordOf resolves the spec into what the guest is told: the ids on the host, the policy in the guest's units.
func recordOf(spec models.SandboxSpec) (record, error) {
	r := record{RootFS: spec.RootFS, Resources: spec.Resources, Run: supervisor.Base{Env: bundle.Environment(spec.Env), WorkDir: spec.WorkDir}}
	r.network(spec)
	// A seed's passwd and CA bundle are on its disk, not in the image, so launch resolves them once the guest is up.
	if spec.Seed != "" {
		// The guest refuses to run as a bare name, so a create cut before launch resolves it never runs as root.
		r.Run.User = spec.User

		return r, nil
	}
	if err := r.identify(spec.RootFS, spec.User, spec.ProxyCA); err != nil {
		return record{}, fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	return r, nil
}

// identify resolves the user, and the trust when there is a proxy CA, against tree: the image rootfs, or a copy of a seed's own files.
func (r *record) identify(tree, user string, proxyCA []byte) error {
	if user != "" {
		identity, err := bundle.ResolveUser(tree, user)
		if err != nil {
			return err
		}
		r.Run.User = fmt.Sprintf("%d:%d", identity.UID, identity.GID)
		r.Run.Groups = identity.Groups
	}
	if proxyCA == nil {
		return nil
	}

	return r.trust(proxyCA)
}

// trust merges the proxy CA into the roots the way the bundle plants them on Linux; the guest writes the store at every start, so a fork's disk carries it too.
func (r *record) trust(proxyCA []byte) error {
	trust, err := r.roots(proxyCA)
	if err != nil {
		return err
	}
	r.Run.Env = runspec.MergeEnv(r.Run.Env, trust.Env)
	r.Run.Trust = &supervisor.Trust{Path: trust.Path, Roots: trust.Roots}

	return nil
}

// network takes the lease and the name from the spec, which a fresh create and a fork both give the guest.
func (r *record) network(spec models.SandboxSpec) {
	if !spec.Network.Address.IsValid() {
		return
	}
	r.Address = spec.Network.Address.String()
	r.Gateway = spec.Network.Gateway.String()
	r.Hostname = spec.Name
	if r.Hostname == "" {
		r.Hostname = spec.ID
	}
	for _, server := range spec.Network.Nameservers {
		r.Nameservers = append(r.Nameservers, server.String())
	}
}

// roots reads the seed's roots the create kept, or the image's.
func (r *record) roots(proxyCA []byte) (bundle.Store, error) {
	if r.Roots != nil {
		return r.Roots.Trust(proxyCA)
	}

	return bundle.Trust(r.RootFS, r.Run.Env, proxyCA)
}

// Start sets the guest up for its processes, over a VM booted again on the disk the stop kept when the last one is gone.
func (p *Provider) Start(ctx context.Context, id string) error {
	dir, r, err := p.open(id)
	if err != nil {
		return err
	}
	if r.Paused {
		return fmt.Errorf("sandbox %s is paused on %s: resume it first", id, Name)
	}

	m, err := p.lookup(ctx, id, dir, r)
	if err != nil {
		return err
	}
	if m != nil {
		status := m.status(p)
		if status.State == models.StateUnresponsive {
			return fmt.Errorf("sandbox %s is %s on %s%s", id, status.State, Name, because(status))
		}
		if status.Alive() {
			return p.setUp(ctx, m, r)
		}
	}
	if err := p.release(ctx, m); err != nil {
		return err
	}

	m, err = p.boot(ctx, id, dir, r, "")
	if err != nil {
		return err
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}

	return p.setUp(ctx, m, r)
}

// setUp gives this boot's guest its work directory and trust, once, before any process.
func (p *Provider) setUp(ctx context.Context, m *machine, r record) error {
	p.mu.Lock()
	started := m.started
	p.mu.Unlock()
	if started {
		return fmt.Errorf("sandbox %s already runs", m.id)
	}
	if err := m.control.Load().Setup(ctx, r.Run.Setup()); err != nil {
		return fmt.Errorf("sandbox %s: set up the guest: %w", m.id, err)
	}
	p.mu.Lock()
	m.started = true
	p.mu.Unlock()

	return nil
}

// open reads the record of a sandbox the provider holds, and names one it does not.
func (p *Provider) open(id string) (string, record, error) {
	dir, err := p.dir(id)
	if err != nil {
		return "", record{}, err
	}
	r, found, err := readRecord(dir)
	if err != nil {
		return "", record{}, err
	}
	if !found {
		return "", record{}, fmt.Errorf("sandbox %s does not exist on %s", id, Name)
	}

	return dir, r, nil
}

// Stop is the only thing that ends a sandbox: the guest gets its stop, the grace, and then the VM is cut.
func (p *Provider) Stop(ctx context.Context, id string, grace time.Duration) error {
	dir, err := p.dir(id)
	if err != nil {
		return err
	}
	r, found, err := readRecord(dir)
	if err != nil || !found {
		return err
	}
	// A save stays where the pause put it; only the record says paused, and no shim holds a saved VM.
	if r.Paused {
		r.Paused = false
		if err := writeRecord(dir, r); err != nil {
			return err
		}
	}

	m, err := p.lookupToStop(ctx, id, dir, r, grace)
	if err != nil || m == nil {
		return err
	}
	if m.status(p).State == models.StateUnresponsive {
		return p.kill(ctx, m)
	}
	if !m.status(p).Alive() {
		return p.release(ctx, m)
	}
	// One deadline covers the request and the wait, so a guest that never answers still gets its forced stop on time (SHARD-339).
	deadline := time.Now().Add(grace)
	stopCtx, cancel := context.WithDeadline(ctx, deadline)
	err = m.control.Load().Stop(stopCtx)
	cancel()
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("stop sandbox %s: %w", id, ctx.Err())
	}
	// The guest forwards TERM to every process and powers off once they are reaped; a refused request is the guest already gone.
	if err != nil && !m.status(p).Alive() {
		return p.release(ctx, m)
	}
	ended, err := m.awaitGone(ctx, time.Until(deadline))
	if err != nil {
		return err
	}
	if ended {
		return p.settle(ctx, m)
	}
	// The grace outran the stop, so the guest flushes its disk before the cut (SHARD-344, shard ruling f4b0942e).
	return p.endLive(ctx, m)
}

// endLive cuts a VM whose guest may still run: it gives the guest a bounded window to flush first, so a forced stop loses nothing a process wrote (SHARD-344).
func (p *Provider) endLive(ctx context.Context, m *machine) error {
	// The guest's flush rides the shim, so a shim too frozen to answer is cut at once and the stop keeps its bound.
	probe, cancelProbe := context.WithTimeout(ctx, probeFloor)
	_, err := m.client.State(probe)
	cancelProbe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vz: sandbox %s: no flush before the forced stop, the shim does not answer: %v\n", m.id, err)

		return p.end(ctx, m)
	}
	// The flush is best effort and off the verb's deadline; a responsive guest syncs within flushGrace, a hung one is cut with the VM anyway.
	flushCtx, cancel := context.WithTimeout(context.Background(), flushGrace)
	defer cancel()
	if err := m.control.Load().Kill(flushCtx); err != nil {
		fmt.Fprintf(os.Stderr, "vz: sandbox %s: flush before the forced stop: %v\n", m.id, err)
	}

	return p.end(ctx, m)
}

// end cuts the VM under the guest, which records no exit; a shim that does not go in time is killed by the pid behind its socket (SHARD-349).
func (p *Provider) end(ctx context.Context, m *machine) error {
	deadline := time.Now().Add(killGrace / 2)
	stopCtx, cancel := context.WithDeadline(ctx, deadline)
	_, stopErr := m.client.Stop(stopCtx)
	cancel()
	// A refused stop is a shim gone, or a frozen one whose full socket queue takes nothing; the wait would hear from neither (SHARD-423).
	if absent(stopErr) {
		return p.kill(ctx, m)
	}
	// A stop whose sandbox ended answers success, so the request's own error counts only when the shim stays.
	ended, err := m.awaitGone(ctx, time.Until(deadline))
	if err != nil {
		return err
	}
	if ended {
		p.forget(m)
		closeDown(m)

		return nil
	}
	if err := p.kill(ctx, m); err != nil {
		return errors.Join(stopErr, err)
	}

	return nil
}

// kill ends the shim by the pid the kernel attests behind its socket, never by a name.
func (p *Provider) kill(ctx context.Context, m *machine) error {
	err := m.client.Kill()
	// A full socket queue refuses the dial that names the pid, and a socket that names another pid proves none, so the shim the attach verified is killed instead (SHARD-423).
	if absent(err) || errors.Is(err, vz.ErrUnproven) {
		err = m.shim.Kill()
	}
	if err != nil {
		return fmt.Errorf("kill the shim of sandbox %s: %w", m.id, err)
	}
	ended, err := m.awaitGone(ctx, killGrace/2)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the shim of sandbox %s still answers %s after a kill", m.id, killGrace/2)
	}
	p.forget(m)
	closeDown(m)

	return nil
}

// Remove ends the VM and drops the disk and the record; the state directory itself is the repository's.
func (p *Provider) Remove(ctx context.Context, id string) error {
	if err := p.Stop(ctx, id, 0); err != nil {
		return err
	}
	dir, err := p.dir(id)
	if err != nil {
		return err
	}
	// A create that failed before its disk landed still holds the reservation.
	bundle.Release(dir)
	for _, name := range []string{diskFile, recordFile, socketFile, shimFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s of sandbox %s: %w", name, id, err)
		}
	}
	// A fork cut inside its capture leaves the save of its source here, which the daemon's start sweeps through this remove.
	if err := os.RemoveAll(filepath.Join(dir, captureDir)); err != nil {
		return fmt.Errorf("remove the capture of sandbox %s: %w", id, err)
	}

	return nil
}

// Snapshot clones the disk a stop kept into dir, sharing its blocks on APFS.
func (p *Provider) Snapshot(ctx context.Context, sourceID, dir string) error {
	sourceDir, _, err := p.open(sourceID)
	if err != nil {
		return err
	}
	status, err := p.Status(ctx, sourceID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s is %s on %s: stop it first, a snapshot copies what a stop kept", sourceID, status.State, Name)
	}

	if _, err := bundle.CloneFile(filepath.Join(sourceDir, diskFile), filepath.Join(dir, diskFile)); err != nil {
		return fmt.Errorf("snapshot the disk of sandbox %s on %s: %w", sourceID, Name, err)
	}

	return nil
}

// cloneDisk copies the disk at src to dst once the root has room for all of it beside the disk of every other sandbox.
func cloneDisk(src, dst string) error {
	return bundle.AdmitCopy(src, dst, func() error {
		_, err := bundle.CloneFile(src, dst)

		return err
	})
}

// lost is the first event the loop could not land, which the files would otherwise answer for as if it never came.
func (p *Provider) lost(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, held := p.machines[id]
	if !held || m.lost == nil {
		return nil
	}

	return fmt.Errorf("sandbox %s %w: %w", id, models.ErrLostState, m.lost)
}

// Status asks the shim, because a record saying running or paused can outlive a restart of the daemon.
func (p *Provider) Status(ctx context.Context, id string) (models.Status, error) {
	dir, err := p.dir(id)
	if err != nil {
		return models.Status{}, err
	}
	r, found, err := readRecord(dir)
	if err != nil {
		return models.Status{}, err
	}
	if !found {
		return models.Status{}, nil
	}

	m, err := p.lookup(ctx, id, dir, r)
	if err != nil {
		return models.Status{}, err
	}
	status := models.Status{Exists: true, State: models.StateStopped, OOMKilled: oomKilled(dir)}
	if m != nil {
		status = m.status(p)
	}
	if status.Alive() {
		return status, nil
	}
	status.SupervisorFailed, err = supervisorFailed(dir)
	if err != nil {
		return models.Status{}, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return status, nil
}

// oomKilled reads the marker the last boot left; only the next boot clears it.
func oomKilled(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, oomFile))

	return err == nil
}

// supervisorFailed reads the reason the last boot's shard-init gave for its own death, empty when it did not die.
func supervisorFailed(dir string) (string, error) {
	reason, err := os.ReadFile(filepath.Join(dir, supervisorFailedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read why the supervisor failed: %w", err)
	}

	return string(reason), nil
}
