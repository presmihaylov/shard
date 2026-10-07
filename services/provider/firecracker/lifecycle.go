package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Create writes the sandbox's overlay disk, records what the VM boots with and boots it; nothing in the guest runs yet.
func (p *Provider) Create(ctx context.Context, spec models.SandboxSpec) error {
	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}
	if spec.BaseDisk == "" {
		return fmt.Errorf("sandbox %s: the image has no erofs image, and a microVM boots from one", spec.ID)
	}
	if err := checkMemory(spec); err != nil {
		return err
	}

	if err := clear(spec.StateDir); err != nil {
		return err
	}
	if err := writeOverlay(spec); err != nil {
		return fmt.Errorf("sandbox %s on %s: %w", spec.ID, Name, err)
	}

	r, err := recordOf(spec)
	if err != nil {
		return err
	}

	return p.launch(ctx, spec, r)
}

// writeOverlay lays down an empty overlay, or a reflink of the seed's grown to the bound.
func writeOverlay(spec models.SandboxSpec) error {
	to := filepath.Join(spec.StateDir, bundle.OverlayDiskFile)
	if spec.Seed == "" {
		return bundle.WriteOverlayDisk(to, spec.Resources)
	}

	from := filepath.Join(spec.Seed, bundle.OverlayDiskFile)

	return bundle.GrowSeed(to, spec.Resources, func() error { return bundle.Reflink(from, to) })
}

// launch records the sandbox, boots its VM, addresses the guest and reads a seed's own files.
func (p *Provider) launch(ctx context.Context, spec models.SandboxSpec, r record) error {
	id, dir := spec.ID, spec.StateDir
	if err := writeRecord(dir, r); err != nil {
		return err
	}

	m, err := p.boot(ctx, id, dir, &r)
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(dir, recordFile)))
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.endAnyway(ctx, m), os.Remove(filepath.Join(dir, recordFile)))
	}
	if spec.Seed != "" {
		r, err = seeded(r, spec, supervisor.GuestFiles(ctx, guestRun(m), bundle.MaxGuestFile))
		if err != nil {
			return errors.Join(err, p.endAnyway(ctx, m), os.Remove(filepath.Join(dir, recordFile)))
		}
		if err := writeRecord(dir, r); err != nil {
			return errors.Join(err, p.endAnyway(ctx, m), os.Remove(filepath.Join(dir, recordFile)))
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

// seeded resolves the user and the CA roots against the seed's own files, which only the booted guest can read off its overlay (SHARD-784).
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
	for _, stale := range []string{oomFile, supervisorFailedFile, supervisor.ProcessTable, recordFile, memoryFile, captureFile, bundle.OverlayDiskFile} {
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
	if res.VCPUs > MaxVCPUs {
		return fmt.Errorf("resources.vcpus is %d, more than the %d vcpus provider %s gives a guest; set it to %d or less", res.VCPUs, MaxVCPUs, Name, MaxVCPUs)
	}
	disk := bundle.DiskBound(res)
	if disk < bundle.MinOverlayDiskMiB {
		return fmt.Errorf("resources.disk_mib is %d MiB, under the %d MiB provider %s needs; set it to %d MiB or more", disk, bundle.MinOverlayDiskMiB, Name, bundle.MinOverlayDiskMiB)
	}
	if err := bundle.CheckGrowBound(disk); err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}

	return nil
}

// recordOf resolves the spec into what the guest is told: the ids on the host, the policy in the guest's units.
func recordOf(spec models.SandboxSpec) (record, error) {
	r := record{BaseDisk: spec.BaseDisk, RootFS: spec.RootFS, Resources: spec.Resources, Run: supervisor.Base{Env: bundle.Environment(spec.Env), WorkDir: spec.WorkDir}}
	r.network(spec)
	// A seed's passwd and CA bundle are on its overlay, not in the image, so launch resolves them once the guest is up.
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

// network takes the tap, the lease and the name from the spec, which every create gives the guest.
func (r *record) network(spec models.SandboxSpec) {
	if !spec.Network.Address.IsValid() {
		return
	}
	r.Tap = spec.Network.HostInterface
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

// trust merges the proxy CA into the roots the way the bundle plants them on Linux; the guest writes the store at every start, so a snapshot's overlay carries it too.
func (r *record) trust(proxyCA []byte) error {
	trust, err := r.roots(proxyCA)
	if err != nil {
		return err
	}
	r.Run.Env = runspec.MergeEnv(r.Run.Env, trust.Env)
	r.Run.Trust = &supervisor.Trust{Path: trust.Path, Roots: trust.Roots}

	return nil
}

// roots reads the seed's roots the create kept, or the image's.
func (r *record) roots(proxyCA []byte) (bundle.Store, error) {
	if r.Roots != nil {
		return r.Roots.Trust(proxyCA)
	}

	return bundle.Trust(r.RootFS, r.Run.Env, proxyCA)
}

// Start sets the guest up for its processes, over a VM booted again on the overlay the stop kept when the last one is gone.
func (p *Provider) Start(ctx context.Context, id string) error {
	dir, r, err := p.open(id)
	if err != nil {
		return err
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
			return p.setUpLive(ctx, m, r)
		}
	}
	// A loss names the run that ended, so a fresh boot leaves it behind once that run's vmm is gone (SHARD-578).
	if err := p.release(ctx, m); err != nil && !errors.Is(err, models.ErrLostState) {
		return err
	}
	if err := p.passLoss(ctx, id); err != nil {
		return err
	}

	m, err = p.boot(ctx, id, dir, &r)
	if err != nil {
		return err
	}
	if err := p.dropLoss(id); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.endAnyway(ctx, m))
	}

	return p.setUp(ctx, m, r)
}

// setUpLive sets up a VM still up, unless its run was lost, which only a fresh boot leaves behind.
func (p *Provider) setUpLive(ctx context.Context, m *machine, r record) error {
	if err := p.lost(m.id); err != nil {
		return err
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

// Stop is the only thing that ends a sandbox: the guest gets its stop, the grace, and then the vmm is killed.
func (p *Provider) Stop(ctx context.Context, id string, grace time.Duration) error {
	dir, err := p.dir(id)
	if err != nil {
		return err
	}
	r, found, err := readRecord(dir)
	if err != nil || !found {
		return err
	}

	m, err := p.lookupToStop(ctx, id, dir, r)
	if err != nil {
		return err
	}
	if m == nil {
		return p.lost(id)
	}
	// A vmm still silent after the lookup's short probe has a guest that cannot hear a stop, so it gets no grace (SHARD-392).
	if m.status(p).State == models.StateUnresponsive {
		return p.endSilent(ctx, m)
	}
	if !m.status(p).Alive() {
		return p.release(ctx, m)
	}
	// One deadline covers the request and the wait, so a guest that never answers still gets its kill on time (SHARD-339).
	deadline := time.Now().Add(grace)
	stopCtx, cancel := context.WithDeadline(ctx, deadline)
	err = m.control.Load().Stop(stopCtx)
	cancel()
	// The guest forwards TERM to every process and reboots once they are reaped; a refused request is the guest already gone.
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("stop sandbox %s: %w", id, ctx.Err())
		}
		if !m.status(p).Alive() {
			return p.release(ctx, m)
		}
		// A guest with no stream, or no answer within the grace, is past waiting for; it still flushes before the cut.
		return p.endLive(ctx, m)
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
	// The guest's flush rides the vmm, so a vmm too frozen to answer is cut at once and the stop keeps its bound.
	probe, cancelProbe := context.WithTimeout(ctx, probeFloor)
	_, err := m.client.State(probe)
	cancelProbe()
	if err != nil {
		// Log and continue, ruled by @shard (SHARD-561): the forced stop below still ends the vmm (SHARD-344 ruling 2).
		fmt.Fprintf(os.Stderr, "firecracker: sandbox %s: no flush before the forced stop, the vmm does not answer: %v\n", m.id, err)

		return p.end(ctx, m)
	}
	// The flush is best effort and off the verb's deadline; a responsive guest syncs within flushGrace, a hung one is cut with the VM anyway.
	flushCtx, cancel := context.WithTimeout(context.Background(), flushGrace)
	defer cancel()
	if err := m.control.Load().Kill(flushCtx); err != nil {
		// Log and continue, ruled by @shard (SHARD-561): the flush is best effort, and the cut below ends the vmm either way (SHARD-344 ruling 2).
		fmt.Fprintf(os.Stderr, "firecracker: sandbox %s: flush before the forced stop: %v\n", m.id, err)
	}

	return p.end(ctx, m)
}

// end kills the vmm under the guest, which records no exit; firecracker has no stop verb, so the kill is the only cut.
func (p *Provider) end(ctx context.Context, m *machine) error {
	if err := m.client.Kill(); err != nil {
		return fmt.Errorf("kill the vmm of sandbox %s: %w", m.id, err)
	}
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the vmm of sandbox %s still answers %s after a kill", m.id, killGrace)
	}

	return p.settle(ctx, m)
}

// endAnyway ends the vmm of a verb that failed after it came up, on its own clock: the failure may be the verb's cut, and the vmm must go either way (SHARD-622).
func (p *Provider) endAnyway(ctx context.Context, m *machine) error {
	return p.end(context.WithoutCancel(ctx), m)
}

// endSilent kills a silent vmm through the pin its attach or adopt took, never by a pid or the socket, either of which may name a vmm begun since.
func (p *Provider) endSilent(ctx context.Context, m *machine) error {
	if err := m.pinned.Kill(); err != nil {
		return fmt.Errorf("sandbox %s: end its silent vmm: %w", m.id, err)
	}
	if err := awaitEnded(m); err != nil {
		return err
	}

	return p.settle(ctx, m)
}

// Remove ends the VM and drops the overlay, the record and the jail, and what a daemon before the jail left; the state directory itself is the repository's.
func (p *Provider) Remove(ctx context.Context, id string) error {
	// A loss comes back only once the vmm is gone, and rm drops it with the files that cannot answer for the run.
	if err := p.Stop(ctx, id, 0); err != nil && !errors.Is(err, models.ErrLostState) {
		return err
	}
	dir, err := p.dir(id)
	if err != nil {
		return err
	}
	// A create that failed before its disk landed still holds the reservation.
	bundle.Release(dir)
	for _, name := range []string{bundle.OverlayDiskFile, memoryFile, recordFile, socketFile, vsockFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s of sandbox %s: %w", name, id, err)
		}
	}
	// The jail is named by the id, so the one a failed create left goes too, with no record to name it.
	if err := removeJail(jailRoot(p.cfg.JailBase, id)); err != nil {
		return err
	}
	if err := p.dropLoss(id); err != nil {
		return err
	}

	// A stopped sandbox keeps its cgroup, empty, because the start that brings it back boots into that one.
	return p.sweep(ctx, id)
}

// Snapshot reflinks the overlay a stop kept into dir; the root of a microVM always reflinks, so a full copy never happens.
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

	from, to := filepath.Join(sourceDir, bundle.OverlayDiskFile), filepath.Join(dir, bundle.OverlayDiskFile)
	if err := bundle.Reflink(from, to); err != nil {
		return fmt.Errorf("snapshot the overlay of sandbox %s on %s: %w", sourceID, Name, err)
	}

	return nil
}

// lost is the first event the loop could not land, which the files would otherwise answer for as if it never came.
func (p *Provider) lost(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cause := p.lostRuns[id].cause
	if m, held := p.machines[id]; held && m.lost != nil {
		cause = m.lost
	}
	if cause == nil {
		return nil
	}

	return lossOf(id, cause)
}

func lossOf(id string, cause error) error {
	return fmt.Errorf("sandbox %s %w: %w", id, models.ErrLostState, cause)
}

// passLoss lets a start past a loss once the pin proves the lost run's vmm exited; a saved pid may name a vmm begun since (SHARD-578).
func (p *Provider) passLoss(ctx context.Context, id string) error {
	p.mu.Lock()
	run, found := p.lostRuns[id]
	p.mu.Unlock()
	if !found {
		return nil
	}
	// The socket goes before the process does, so the exit gets the grace a kill gets.
	deadline := time.Now().Add(killGrace)
	for {
		exited, err := run.pin.Exited()
		if err != nil {
			return errors.Join(lossOf(id, run.cause), err)
		}
		if exited {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w, and its vmm has not exited %s after it went", lossOf(id, run.cause), killGrace)
		}
		select {
		case <-ctx.Done():
			return errors.Join(lossOf(id, run.cause), ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// dropLoss forgets a loss and lets the pin of its vmm go.
func (p *Provider) dropLoss(id string) error {
	p.mu.Lock()
	run, found := p.lostRuns[id]
	delete(p.lostRuns, id)
	p.mu.Unlock()
	if !found {
		return nil
	}
	if err := run.pin.Close(); err != nil {
		return fmt.Errorf("sandbox %s: let the pin of its lost vmm go: %w", id, err)
	}

	return nil
}

// Status asks the vmm, because a record saying running can outlive a restart of the daemon.
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
	// A run whose last report never landed has nothing true on file, so it ends as a supervisor failure and never as an ordinary death.
	if lost := p.lost(id); lost != nil {
		status.SupervisorFailed = lost.Error()

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
