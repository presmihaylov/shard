// Package gvisor runs sandboxes on gVisor by driving bare runsc. Pause, resume and fork land in
// chunk 3; until then this provider refuses them rather than downgrading to a weaker mechanism.
package gvisor

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
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
)

const name = "gvisor"

// logFile holds the guest's stdout and stderr, interleaved the way a terminal would show them.
const logFile = "output.log"

const (
	// pollInterval paces every wait here. runsc state is a socket round trip, so it is not free.
	pollInterval = 100 * time.Millisecond
	// killGrace bounds the wait after SIGKILL, which the sentry cannot refuse.
	killGrace = 10 * time.Second
)

// StateDirs answers where a sandbox's directory is. sandboxstate.Repository.Dir is what shard passes:
// every verb below takes an id, and shard runs no daemon that could remember the path from Create.
type StateDirs func(id string) (string, error)

var _ models.Provider = (*Provider)(nil)

// Provider implements models.Provider on gVisor.
type Provider struct {
	runsc   *runsc.Runner
	bundles *bundle.Service
	dirs    StateDirs
}

func New(runner *runsc.Runner, bundles *bundle.Service, dirs StateDirs) (*Provider, error) {
	if runner == nil || bundles == nil || dirs == nil {
		return nil, errors.New("the gvisor provider needs a runsc runner, a bundle service and a state directory lookup")
	}

	return &Provider{runsc: runner, bundles: bundles, dirs: dirs}, nil
}

func (p *Provider) Name() string { return name }

// Capabilities is empty until chunk 3: SHARD-32 adds pause and resume, SHARD-33 adds fork.
func (p *Provider) Capabilities() models.Capabilities { return models.Capabilities{} }

// Create builds the bundle, stacks the writable layer over the image and prepares the container.
func (p *Provider) Create(ctx context.Context, spec models.SandboxSpec) (models.Runtime, error) {
	b, err := p.bundles.Build(spec)
	if err != nil {
		return models.Runtime{}, err
	}

	if err := b.Mount(); err != nil {
		return models.Runtime{}, err
	}

	runtime, err := p.create(ctx, spec, b)
	if err != nil {
		// A half-created sandbox must not leave a mount behind, because nothing else knows to drop it.
		return models.Runtime{}, errors.Join(err, b.Unmount())
	}

	return runtime, nil
}

func (p *Provider) create(ctx context.Context, spec models.SandboxSpec, b bundle.Bundle) (runtime models.Runtime, err error) {
	// A create over a state directory that already ran must not let the previous run's status answer a wait.
	if err := os.Remove(b.ExitFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return models.Runtime{}, fmt.Errorf("clear %s: %w", b.ExitFile, err)
	}

	out, err := openLog(filepath.Join(spec.StateDir, logFile))
	if err != nil {
		return models.Runtime{}, err
	}
	// The sandbox keeps its own copy of the fd, so closing ours does not cut the guest's output off.
	defer func() { err = errors.Join(err, out.Close()) }()

	if err := p.runsc.Create(ctx, spec.ID, runsc.CreateOptions{Bundle: b.Dir, Stdout: out, Stderr: out}); err != nil {
		return models.Runtime{}, err
	}

	state, err := p.runsc.State(ctx, spec.ID)
	if err != nil {
		return models.Runtime{}, err
	}

	// The veth is the network service's, not the provider's: gVisor only joins the namespace it is given.
	return models.Runtime{PID: state.PID, HostInterface: spec.Network.HostInterface}, nil
}

// Start runs the entrypoint under the supervisor that runsc create already made PID 1. A stopped
// sandbox never starts again: runsc refuses it, so a second run goes through Remove and Create, which
// keeps the writable layer the state directory holds.
func (p *Provider) Start(ctx context.Context, id string) error {
	return p.runsc.Start(ctx, id)
}

// Stop is the only thing that ends a sandbox. It signals, waits out grace, then kills.
func (p *Provider) Stop(ctx context.Context, id string, grace time.Duration) error {
	state, err := p.runsc.State(ctx, id)
	if err != nil && !errors.Is(err, runsc.ErrNotFound) {
		return err
	}

	// runsc refuses to signal a container whose entrypoint never started, so only a delete ends that one.
	if state.Status == runsc.StatusCreated {
		if err := p.delete(ctx, id); err != nil {
			return err
		}

		return p.unmount(id)
	}

	// TERM goes to PID 1, which is shard-init: it forwards the signal to the entrypoint and then exits.
	if err := p.runsc.Kill(ctx, id, "TERM", false); err != nil && !gone(err) {
		return err
	}

	stopped, err := p.awaitStopped(ctx, id, grace)
	if err != nil {
		return err
	}

	if !stopped {
		if err := p.kill(ctx, id); err != nil {
			return err
		}
	}

	return p.unmount(id)
}

func (p *Provider) kill(ctx context.Context, id string) error {
	if err := p.runsc.Kill(ctx, id, "KILL", true); err != nil && !gone(err) {
		return err
	}

	stopped, err := p.awaitStopped(ctx, id, killGrace)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("sandbox %s is still running %s after SIGKILL", id, killGrace)
	}

	return nil
}

// Remove deletes runsc's own state. The record and the state directory belong to the repository.
func (p *Provider) Remove(ctx context.Context, id string) error {
	if err := p.delete(ctx, id); err != nil {
		return err
	}

	// The repository removes the directory after this, and it must never remove a live mount.
	return p.unmount(id)
}

// delete drops what runsc holds. --force, because a sandbox that is still running holds the rootfs.
func (p *Provider) delete(ctx context.Context, id string) error {
	if err := p.runsc.Delete(ctx, id, true); err != nil && !errors.Is(err, runsc.ErrNotFound) {
		return err
	}

	return nil
}

// unmount drops the merged view. The upper layer stays, which is what a later create reads back.
func (p *Provider) unmount(id string) error {
	b, err := p.open(id)
	if err != nil {
		return err
	}

	return b.Unmount()
}

// gone reports whether a signal failed because the sandbox had already ended, which is what a stop wants.
func gone(err error) bool {
	return errors.Is(err, runsc.ErrNotRunning) || errors.Is(err, runsc.ErrNotFound)
}

// Wait blocks until the entrypoint exits. runsc wait cannot serve it: PID 1 is the supervisor and it
// never exits, so runsc wait would block forever. Watch the file shard-init writes instead.
func (p *Provider) Wait(ctx context.Context, id string) (models.ExitStatus, error) {
	b, err := p.open(id)
	if err != nil {
		return models.ExitStatus{}, err
	}

	for {
		status, found, err := readExitStatus(b.ExitFile)
		if err != nil {
			return models.ExitStatus{}, err
		}
		if found {
			return status, nil
		}

		alive, err := p.Alive(ctx, id)
		if err != nil {
			return models.ExitStatus{}, err
		}
		if !alive {
			// The supervisor may have written the file between the read above and this check.
			return lastExitStatus(b.ExitFile, id)
		}

		select {
		case <-ctx.Done():
			return models.ExitStatus{}, fmt.Errorf("wait for the entrypoint of %s: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Alive asks the substrate, because a record saying running can outlive a shard restart.
func (p *Provider) Alive(ctx context.Context, id string) (bool, error) {
	state, err := p.runsc.State(ctx, id)
	if errors.Is(err, runsc.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return state.Status != runsc.StatusStopped, nil
}

func (p *Provider) Pause(ctx context.Context, id string, dir string) error {
	return models.Unsupported(name, "pause")
}

func (p *Provider) Resume(ctx context.Context, id string, dir string) error {
	return models.Unsupported(name, "resume")
}

func (p *Provider) Fork(ctx context.Context, dir string, spec models.SandboxSpec) (models.Runtime, error) {
	return models.Runtime{}, models.Unsupported(name, "fork")
}

// LogPath is where the guest's stdout and stderr land. SHARD-23 turns it into shard logs.
func (p *Provider) LogPath(id string) (string, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, logFile), nil
}

// open finds the bundle of a sandbox this process did not create.
func (p *Provider) open(id string) (bundle.Bundle, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return bundle.Bundle{}, err
	}

	return bundle.Open(dir)
}

// awaitStopped reports whether the sandbox ended within the budget. It never fails on a missing one:
// a removed sandbox is stopped by any definition a caller cares about.
func (p *Provider) awaitStopped(ctx context.Context, id string, budget time.Duration) (bool, error) {
	deadline := time.Now().Add(budget)

	for {
		alive, err := p.Alive(ctx, id)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}

		select {
		case <-ctx.Done():
			return false, fmt.Errorf("wait for sandbox %s to stop: %w", id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// openLog appends, so a second create over the same state directory adds to the sandbox's output.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	return f, nil
}

// readExitStatus reads what shard-init wrote. The file arrives by rename, so it never reads half of one.
func readExitStatus(path string) (models.ExitStatus, bool, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return models.ExitStatus{}, false, nil
	}
	if err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	var status models.ExitStatus
	if err := json.Unmarshal(blob, &status); err != nil {
		return models.ExitStatus{}, false, fmt.Errorf("decode the exit status in %s: %w", path, err)
	}

	return status, true, nil
}

// lastExitStatus answers a wait on a sandbox that has already ended, which only Stop can have done.
func lastExitStatus(path, id string) (models.ExitStatus, error) {
	status, found, err := readExitStatus(path)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if !found {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s ended before its entrypoint exited", id)
	}

	return status, nil
}
