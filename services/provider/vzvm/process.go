package vzvm

import (
	"context"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// StartProcess runs one named process in the guest, where an exec runs, and returns once it forked.
func (p *Provider) StartProcess(ctx context.Context, id string, spec models.ProcessSpec) error {
	m, r, err := p.running(ctx, id)
	if err != nil {
		return err
	}
	if err := refuseHeld(m, "the run"); err != nil {
		return err
	}
	if err := m.host.Outdated(id); err != nil {
		return err
	}
	if err := m.control.Load().Run(ctx, r.Run.RunOf(spec)); err != nil {
		return supervisor.ProcessError(id, err)
	}
	m.host.Follow(spec.Name)

	return nil
}

// StopProcess terms one named process and kills it once grace passes, and returns once the guest reaped it.
func (p *Provider) StopProcess(ctx context.Context, id, name string, grace time.Duration) error {
	m, _, err := p.running(ctx, id)
	if err != nil {
		return err
	}
	if err := refuseHeld(m, "the process stop"); err != nil {
		return err
	}
	if err := m.host.Outdated(id); err != nil {
		return err
	}
	if err := m.control.Load().StopProcess(ctx, name, grace); err != nil {
		return supervisor.ProcessError(id, err)
	}

	return nil
}

// Processes is the guest's last report of each process, from the table the host keeps, so a stopped sandbox answers too.
func (p *Provider) Processes(_ context.Context, id string) ([]models.ProcessReport, error) {
	dir, _, err := p.open(id)
	if err != nil {
		return nil, err
	}
	if err := p.lost(id); err != nil {
		return nil, err
	}
	table, err := supervisor.ReadProcesses(dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}

	return table, nil
}

// ProcessLogPath names the file the host lands one process's output in, off the guest's logs port.
func (p *Provider) ProcessLogPath(id, name string) (string, error) {
	if !models.ValidProcessName(name) {
		return "", fmt.Errorf("sandbox %s: the process name %q is not lowercase letters, digits, '.', '_' or '-', at most %d long", id, name, models.MaxProcessName)
	}
	dir, err := p.dir(id)
	if err != nil {
		return "", err
	}

	return supervisor.ProcessLogPath(dir, name), nil
}
