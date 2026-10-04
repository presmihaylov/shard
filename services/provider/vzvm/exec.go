package vzvm

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Exec runs one command in a sandbox that already runs, over its own vsock connection to shard-init.
func (p *Provider) Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if len(spec.Argv) == 0 {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: exec has no command to run", id)
	}

	// A dial would wait on the save or meet VZ's raw refusal of a paused VM, so the pause refuses by name, as the frozen guest does (SHARD-478).
	if p.pausing(id) {
		return models.ExitStatus{}, &models.CommandNotStartedError{Sandbox: id, Reason: fmt.Sprintf("a %s holds the sandbox frozen, and nothing starts in it until that ends: run the command again", models.VerbPause), Code: models.CommandNotExecutableExitCode}
	}

	m, r, err := p.running(ctx, id)
	if err != nil {
		return models.ExitStatus{}, err
	}

	header, err := headerOf(r, spec)
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return supervisor.Exec(ctx, m.dial, id, header, spec)
}

// pausing says a pause holds the sandbox's VM, from its freeze until it stops the VM or runs it again.
func (p *Provider) pausing(id string) bool {
	p.mu.Lock()
	m, held := p.machines[id]
	p.mu.Unlock()

	return held && m.pausing.Load()
}

// running finds the sandbox's live guest, and refuses anything else by name and state, as an exec needs.
func (p *Provider) running(ctx context.Context, id string) (*machine, record, error) {
	dir, err := p.dir(id)
	if err != nil {
		return nil, record{}, err
	}
	r, found, err := readRecord(dir)
	if err != nil {
		return nil, record{}, err
	}
	if !found {
		return nil, record{}, fmt.Errorf("sandbox %s does not exist on %s", id, Name)
	}
	if r.Paused {
		return nil, record{}, fmt.Errorf("sandbox %s is %s on %s, so nothing can run in it", id, models.StatePaused, Name)
	}

	m, err := p.lookup(ctx, id, dir, r)
	if err != nil {
		return nil, record{}, err
	}
	status := models.Status{State: models.StateStopped}
	if m != nil {
		status = m.status(p)
	}
	if status.State != models.StateRunning {
		return nil, record{}, fmt.Errorf("sandbox %s is %s on %s, so nothing can run in it%s", id, status.State, Name, because(status))
	}

	return m, r, nil
}

// headerOf puts the exec where the entrypoint runs: its env under the overrides, its workdir and its user unless the spec names one.
func headerOf(r record, spec models.ExecSpec) (supervisor.ExecHeader, error) {
	header := supervisor.ExecHeader{
		Argv:    spec.Argv,
		Env:     runspec.MergeEnv(r.Run.Env, spec.Env),
		WorkDir: firstNonEmpty(spec.WorkDir, r.Run.WorkDir, "/"),
		User:    r.Run.User,
		Groups:  r.Run.Groups,
		TTY:     spec.TTY,
	}
	// The guest resolves a named user, because the image on the host misses a user the sandbox added (SHARD-356).
	if spec.User != "" {
		header.User, header.Groups, header.Lookup = spec.User, nil, true
	}
	if spec.TTY && spec.Stdin != nil {
		size, err := pty.SizeOf(spec.Stdin)
		if err != nil {
			return supervisor.ExecHeader{}, fmt.Errorf("read the terminal size: %w", err)
		}
		header.Rows, header.Cols = size.Rows, size.Cols
	}

	return header, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// Signal sends one signal to a running exec by the guest pid the exec reported for it.
func (p *Provider) Signal(ctx context.Context, id string, pid int, signal string) error {
	m, _, err := p.running(ctx, id)
	if err != nil {
		return err
	}
	if err := m.control.Load().Signal(ctx, pid, signal); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	return nil
}

// StopApp asks the guest supervisor to cancel every start again and term the app, or kill it with force.
func (p *Provider) StopApp(ctx context.Context, id string, force bool) error {
	m, _, err := p.running(ctx, id)
	if err != nil {
		return err
	}
	if err := m.control.Load().StopApp(ctx, force); err != nil {
		return fmt.Errorf("sandbox %s: stop the app: %w", id, err)
	}

	return nil
}
