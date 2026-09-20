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
	if _, err := bundle.CloneRootDisk(spec.RootDisk, filepath.Join(spec.StateDir, diskFile), spec.Resources); err != nil {
		return fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	r, err := recordOf(spec)
	if err != nil {
		return err
	}

	return p.launch(ctx, spec.ID, spec.StateDir, r, false)
}

// launch records the sandbox, boots its VM, addresses the guest, and runs the entrypoint when asked.
func (p *Provider) launch(ctx context.Context, id, dir string, r record, run bool) error {
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
	if err := m.readdress(r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}
	if !run {
		return nil
	}

	return p.run(m, r)
}

// clear drops what an earlier run of this state directory left, so nothing of it answers for the new one.
func clear(dir string) error {
	for _, stale := range []string{exitFile, restartsFile, oomFile, logFile, recordFile, diskFile} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	return nil
}

// recordOf resolves the spec into what the guest is told: the ids on the host, the policy in the guest's units.
// checkMemory refuses a bound the guest cannot boot under; zero is unbounded on Linux, and a VM has no such thing.
func checkMemory(spec models.SandboxSpec) error {
	if spec.Resources.MemoryMiB == 0 {
		return fmt.Errorf("sandbox %s: provider %s takes no --memory 0, a VM's memory is real memory on the host; set --memory <MiB>, %d or more", spec.ID, Name, MinMemoryMiB)
	}
	if spec.Resources.MemoryMiB < MinMemoryMiB {
		return fmt.Errorf("sandbox %s: %s needs at least %d MiB of memory, got %d", spec.ID, Name, MinMemoryMiB, spec.Resources.MemoryMiB)
	}

	return nil
}

func recordOf(spec models.SandboxSpec) (record, error) {
	run, err := runOf(spec.RootFS, spec.Entrypoint, spec.Env, spec.WorkDir, spec.User, spec.Restart)
	if err != nil {
		return record{}, fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}
	r := record{RootFS: spec.RootFS, Resources: spec.Resources, Run: run}
	r.network(spec)
	if spec.ProxyCA != nil {
		if err := r.trust(spec.ProxyCA); err != nil {
			return record{}, fmt.Errorf("sandbox %s: %w", spec.ID, err)
		}
	}

	return r, nil
}

// trust merges the proxy CA into the image roots the way the bundle plants them on Linux; the guest writes the store at every start, so a fork's disk carries it too.
func (r *record) trust(proxyCA []byte) error {
	trust, err := bundle.Trust(r.RootFS, r.Run.Env, proxyCA)
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

// Start runs the entrypoint, over a VM booted again on the disk the stop kept when the last one is gone.
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
	if m != nil && m.status(p).Alive() {
		return p.run(m, r)
	}
	if err := p.release(ctx, m); err != nil {
		return err
	}

	m, err = p.boot(ctx, id, dir, r, "")
	if err != nil {
		return err
	}
	if err := m.readdress(r); err != nil {
		return errors.Join(err, p.end(ctx, m))
	}

	return p.run(m, r)
}

// run asks the guest to fork the entrypoint; the guest answers once it has, or with why it could not.
func (p *Provider) run(m *machine, r record) error {
	p.mu.Lock()
	started := m.started
	p.mu.Unlock()
	if started {
		return fmt.Errorf("the entrypoint of sandbox %s already runs", m.id)
	}
	if err := m.control.Load().Run(r.Run); err != nil {
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

	m, err := p.lookup(ctx, id, dir, r)
	if err != nil || m == nil {
		return err
	}
	if !m.status(p).Alive() {
		return p.release(ctx, m)
	}
	// The guest forwards TERM to the entrypoint and powers off once it is reaped; a refused request is the guest already gone.
	if err := m.control.Load().Stop(); err != nil && !m.status(p).Alive() {
		return p.release(ctx, m)
	}
	ended, err := m.awaitGone(ctx, grace)
	if err != nil {
		return err
	}
	if ended {
		p.forget(m)

		return m.close()
	}

	return p.end(ctx, m)
}

// end cuts the VM under the guest, which records no exit, and waits for the shim to go.
func (p *Provider) end(ctx context.Context, m *machine) error {
	if _, err := m.client.Stop(); err != nil && !absent(err) {
		return fmt.Errorf("stop the vm of sandbox %s: %w", m.id, err)
	}
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the vm of sandbox %s still runs %s after a forced stop", m.id, killGrace)
	}
	p.forget(m)

	return m.close()
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
	for _, name := range []string{diskFile, recordFile, socketFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s of sandbox %s: %w", name, id, err)
		}
	}

	return nil
}

// Clone boots a new VM over a copy of the source's disk and runs the source's entrypoint from the beginning.
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
	if _, err := bundle.CloneFile(filepath.Join(sourceDir, diskFile), filepath.Join(spec.StateDir, diskFile)); err != nil {
		return fmt.Errorf("copy the disk of sandbox %s: %w", sourceID, err)
	}

	// The spec names the copy and its lease alone; the run is the source's, as the bundle it copies is on Linux.
	r := record{RootFS: src.RootFS, Resources: spec.Resources, Run: src.Run}
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

// Restarts is a file read, not a shim call, so a task may poll it every second.
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

// lost is the first event the loop could not land, which the files would otherwise answer for as if it never came.
func (p *Provider) lost(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, held := p.machines[id]
	if !held || m.lost == nil {
		return nil
	}

	return fmt.Errorf("sandbox %s lost its lifecycle state: %w", id, m.lost)
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
	if m == nil {
		return models.Status{Exists: true, State: models.StateStopped, OOMKilled: oomKilled(dir)}, nil
	}

	return m.status(p), nil
}

// oomKilled reads the marker the last boot left; only the next boot clears it.
func oomKilled(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, oomFile))

	return err == nil
}
