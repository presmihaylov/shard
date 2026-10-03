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
	if err := bundle.WriteOverlayDisk(filepath.Join(spec.StateDir, bundle.OverlayDiskFile), spec.Resources); err != nil {
		return fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	r, err := recordOf(spec)
	if err != nil {
		return err
	}

	return p.launch(ctx, spec.ID, spec.StateDir, r, false)
}

// launch records the sandbox, boots its VM, and runs the entrypoint when asked.
func (p *Provider) launch(ctx context.Context, id, dir string, r record, run bool) error {
	if err := writeRecord(dir, r); err != nil {
		return err
	}

	m, err := p.boot(ctx, id, dir, r)
	if err != nil {
		return errors.Join(err, os.Remove(filepath.Join(dir, recordFile)))
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.end(ctx, m), os.Remove(filepath.Join(dir, recordFile)))
	}
	if !run {
		return nil
	}

	return p.run(ctx, m, r)
}

// clear drops what an earlier run of this state directory left, so nothing of it answers for the new one.
func clear(dir string) error {
	for _, stale := range []string{exitFile, restartsFile, oomFile, supervisorFailedFile, logFile, cursorFile, recordFile, memoryFile, bundle.OverlayDiskFile} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
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
		return fmt.Errorf("provider %s takes no --memory 0, a VM's memory is real memory on the host; set --memory <MiB>, %d or more", Name, MinMemoryMiB)
	}
	if res.MemoryMiB < MinMemoryMiB {
		return fmt.Errorf("%s needs at least %d MiB of memory, got %d", Name, MinMemoryMiB, res.MemoryMiB)
	}
	if res.VCPUs > MaxVCPUs {
		return fmt.Errorf("%s gives a guest at most %d vcpus, got %d", Name, MaxVCPUs, res.VCPUs)
	}
	disk := bundle.DiskBound(res)
	if disk < bundle.MinOverlayDiskMiB {
		return fmt.Errorf("%s needs at least %d MiB of disk, got %d", Name, bundle.MinOverlayDiskMiB, disk)
	}
	if err := bundle.CheckGrowBound(disk); err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}

	return nil
}

// recordOf resolves the spec into what the guest is told: the ids on the host, the policy in the guest's units.
func recordOf(spec models.SandboxSpec) (record, error) {
	run, err := runOf(spec.RootFS, spec.Entrypoint, spec.Env, spec.WorkDir, spec.User, spec.Restart)
	if err != nil {
		return record{}, fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}
	r := record{BaseDisk: spec.BaseDisk, RootFS: spec.RootFS, Resources: spec.Resources, Run: run}
	r.network(spec)
	if spec.ProxyCA != nil {
		if err := r.trust(spec.ProxyCA); err != nil {
			return record{}, fmt.Errorf("sandbox %s: %w", spec.ID, err)
		}
	}

	return r, nil
}

// network takes the tap, the lease and the name from the spec, which a fresh create and a clone both give the guest.
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

// trust merges the proxy CA into the image roots the way the bundle plants them on Linux; the guest writes the store at every start, so a clone's overlay carries it too.
func (r *record) trust(proxyCA []byte) error {
	trust, err := bundle.Trust(r.RootFS, r.Run.Env, proxyCA)
	if err != nil {
		return err
	}
	r.Run.Env = runspec.MergeEnv(r.Run.Env, trust.Env)
	r.Run.Trust = &supervisor.Trust{Path: trust.Path, Roots: trust.Roots}

	return nil
}

func runOf(rootfs string, argv, env []string, workDir, user string, restart models.RestartSpec) (supervisor.RunSpec, error) {
	run := supervisor.RunSpec{
		Argv:    argv,
		Env:     bundle.Environment(env),
		WorkDir: workDir,
		Restart: restart.Policy,
		Retries: restart.Retries,
		Backoff: time.Duration(restart.Backoff) * time.Second,
	}
	if user == "" {
		return run, nil
	}

	identity, err := bundle.ResolveUser(rootfs, user)
	if err != nil {
		return supervisor.RunSpec{}, err
	}
	run.User = fmt.Sprintf("%d:%d", identity.UID, identity.GID)
	run.Groups = identity.Groups

	return run, nil
}

// Start runs the entrypoint, over a VM booted again on the overlay the stop kept when the last one is gone.
func (p *Provider) Start(ctx context.Context, id string) error {
	dir, r, err := p.open(id)
	if err != nil {
		return err
	}
	if err := p.lost(id); err != nil {
		return err
	}

	m, err := p.lookup(ctx, id, dir)
	if err != nil {
		return err
	}
	if m != nil && m.status(p).Alive() {
		return p.run(ctx, m, r)
	}
	if err := p.release(ctx, m); err != nil {
		return err
	}

	m, err = p.boot(ctx, id, dir, r)
	if err != nil {
		return err
	}
	if err := m.readdress(ctx, r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}

	return p.run(ctx, m, r)
}

// run asks the guest to fork the entrypoint; the guest answers once it has, or with why it could not.
func (p *Provider) run(ctx context.Context, m *machine, r record) error {
	p.mu.Lock()
	started := m.started
	p.mu.Unlock()
	if started {
		return fmt.Errorf("the entrypoint of sandbox %s already runs", m.id)
	}
	if err := m.control.Load().Run(ctx, r.Run); err != nil {
		return fmt.Errorf("sandbox %s: %w", m.id, err)
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
	_, found, err := readRecord(dir)
	if err != nil || !found {
		return err
	}

	m, err := p.lookup(ctx, id, dir)
	if err != nil {
		return err
	}
	if m == nil {
		return p.lost(id)
	}
	if !m.status(p).Alive() {
		return p.release(ctx, m)
	}
	// One deadline covers the request and the wait, so a guest that never answers still gets its kill on time (SHARD-339).
	deadline := time.Now().Add(grace)
	stopCtx, cancel := context.WithDeadline(ctx, deadline)
	err = m.control.Load().Stop(stopCtx)
	cancel()
	// The guest forwards TERM to the entrypoint and reboots once it is reaped; a refused request is the guest already gone.
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("stop sandbox %s: %w", id, ctx.Err())
		}
		if !m.status(p).Alive() {
			return p.release(ctx, m)
		}
		// A guest with no stream, or no answer within the grace, is past waiting for.
		return p.end(ctx, m)
	}
	ended, err := m.awaitGone(ctx, time.Until(deadline))
	if err != nil {
		return err
	}
	if ended {
		return p.settle(ctx, m)
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

// Remove ends the VM and drops the overlay, the memory, the record and the sockets; the state directory itself is the repository's.
func (p *Provider) Remove(ctx context.Context, id string) error {
	// A loss comes back only once the vmm is gone, and rm drops it with the files that cannot answer for the run.
	if err := p.Stop(ctx, id, 0); err != nil && !errors.Is(err, errLostState) {
		return err
	}
	dir, err := p.dir(id)
	if err != nil {
		return err
	}
	for _, name := range []string{bundle.OverlayDiskFile, memoryFile, recordFile, socketFile, vsockFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s of sandbox %s: %w", name, id, err)
		}
	}
	p.mu.Lock()
	delete(p.lostRuns, id)
	p.mu.Unlock()

	// A stopped sandbox keeps its cgroup, empty, because the start that brings it back boots into that one.
	return p.sweep(ctx, id)
}

// Clone boots a new VM over a copy of the source's overlay, on the same image, and runs the source's entrypoint from the beginning.
func (p *Provider) Clone(ctx context.Context, sourceID string, spec models.SandboxSpec) error {
	sourceDir, src, err := p.open(sourceID)
	if err != nil {
		return err
	}
	sourceStatus, err := p.Status(ctx, sourceID)
	if err != nil {
		return err
	}
	if sourceStatus.Alive() {
		return fmt.Errorf("sandbox %s is %s on %s: stop it first, clone copies what a stop kept", sourceID, sourceStatus.State, Name)
	}

	status, err := p.Status(ctx, spec.ID)
	if err != nil {
		return err
	}
	if status.Alive() {
		return fmt.Errorf("sandbox %s already exists on %s and is %s", spec.ID, Name, status.State)
	}
	if err := checkMemory(spec); err != nil {
		return err
	}

	if err := clear(spec.StateDir); err != nil {
		return err
	}
	// A clone shares the source's blocks or is refused: a full copy of the overlay is not what the verb promises.
	if err := bundle.Reflink(filepath.Join(sourceDir, bundle.OverlayDiskFile), filepath.Join(spec.StateDir, bundle.OverlayDiskFile)); err != nil {
		return fmt.Errorf("clone the overlay of sandbox %s on %s: %w", sourceID, Name, err)
	}

	// The spec names the copy alone; the image and the run are the source's, as the bundle it copies is on Linux.
	r := record{BaseDisk: src.BaseDisk, RootFS: src.RootFS, Resources: spec.Resources, Run: src.Run}
	r.network(spec)

	return p.launch(ctx, spec.ID, spec.StateDir, r, true)
}

// Wait blocks until the entrypoint exits, by the file the event loop lands each exit in.
func (p *Provider) Wait(ctx context.Context, id string) (models.ExitStatus, error) {
	dir, _, err := p.open(id)
	if err != nil {
		return models.ExitStatus{}, err
	}
	path := filepath.Join(dir, exitFile)

	for {
		if err := p.lost(id); err != nil {
			return models.ExitStatus{}, err
		}
		exit, found, err := bundle.ReadExitStatus(path)
		if err != nil {
			return models.ExitStatus{}, err
		}
		if found {
			return exit, nil
		}

		status, err := p.Status(ctx, id)
		if err != nil {
			return models.ExitStatus{}, err
		}
		if !status.Alive() {
			// The event loop may have landed the exit between the read above and this check.
			return lastExitStatus(path, id)
		}

		select {
		case <-ctx.Done():
			return models.ExitStatus{}, fmt.Errorf("wait for the entrypoint of %s: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// lastExitStatus answers a wait on a sandbox that has already ended, which only Stop can have done.
func lastExitStatus(path, id string) (models.ExitStatus, error) {
	exit, found, err := bundle.ReadExitStatus(path)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if !found {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: %w", id, models.ErrNoExitStatus)
	}

	return exit, nil
}

// ExitStatus reads how the entrypoint ended so far, nil while it still runs.
func (p *Provider) ExitStatus(_ context.Context, id string) (*models.ExitStatus, error) {
	dir, _, err := p.open(id)
	if err != nil {
		return nil, err
	}
	if err := p.lost(id); err != nil {
		return nil, err
	}

	exit, found, err := bundle.ReadExitStatus(filepath.Join(dir, exitFile))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	return &exit, nil
}

// Restarts is a file read, not a vmm call, so a task may poll it every second.
func (p *Provider) Restarts(_ context.Context, id string) (models.RestartCount, error) {
	dir, _, err := p.open(id)
	if err != nil {
		return models.RestartCount{}, err
	}
	if err := p.lost(id); err != nil {
		return models.RestartCount{}, err
	}

	return bundle.Bundle{RestartFile: filepath.Join(dir, restartsFile)}.RestartCount()
}

// errLostState marks a run whose files say nothing true, since the loop could not land one of its events.
var errLostState = errors.New("lost its lifecycle state")

// lost is the first event the loop could not land, which the files would otherwise answer for as if it never came.
func (p *Provider) lost(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cause := p.lostRuns[id]
	if m, held := p.machines[id]; held && m.lost != nil {
		cause = m.lost
	}
	if cause == nil {
		return nil
	}

	return fmt.Errorf("sandbox %s %w: %w", id, errLostState, cause)
}

// Status asks the vmm, because a record saying running can outlive a restart of the daemon.
func (p *Provider) Status(ctx context.Context, id string) (models.Status, error) {
	dir, err := p.dir(id)
	if err != nil {
		return models.Status{}, err
	}
	_, found, err := readRecord(dir)
	if err != nil {
		return models.Status{}, err
	}
	if !found {
		return models.Status{}, nil
	}

	m, err := p.lookup(ctx, id, dir)
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
