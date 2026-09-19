package vzvm

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/supervisor"
)

// Exec runs one command in a sandbox that already runs, over its own vsock connection to shard-init.
func (p *Provider) Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	if len(spec.Argv) == 0 {
		return models.ExitStatus{}, fmt.Errorf("sandbox %s: exec has no command to run", id)
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
	state := models.StateStopped
	if m != nil {
		state = m.status(p).State
	}
	if state != models.StateRunning {
		return nil, record{}, fmt.Errorf("sandbox %s is %s on %s, so nothing can run in it", id, state, Name)
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
	if spec.User != "" {
		identity, err := bundle.ResolveUser(r.RootFS, spec.User)
		if err != nil {
			return supervisor.ExecHeader{}, err
		}
		header.User = fmt.Sprintf("%d:%d", identity.UID, identity.GID)
		header.Groups = identity.Groups
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
	if err := m.control.Signal(pid, signal); err != nil {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}

	return nil
}
