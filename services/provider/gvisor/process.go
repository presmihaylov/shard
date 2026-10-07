package gvisor

import (
	"context"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// StartProcess hands the run to shard-init through an exec as root, with the env, workdir and user an exec defaults to.
func (p *Provider) StartProcess(ctx context.Context, id string, spec models.ProcessSpec) error {
	b, err := p.open(id)
	if err != nil {
		return err
	}
	rt, err := b.Runtime()
	if err != nil {
		return err
	}
	if err := supervisor.RunProcess(ctx, p.execIn(id), rt.RunOf(spec)); err != nil {
		return supervisor.ProcessError(id, err)
	}

	return nil
}

// StopProcess hands the stop to shard-init, which returns once it reaped the process.
func (p *Provider) StopProcess(ctx context.Context, id, name string, grace time.Duration) error {
	if err := supervisor.StopProcess(ctx, p.execIn(id), name, grace); err != nil {
		return supervisor.ProcessError(id, err)
	}

	return nil
}

func (p *Provider) execIn(id string) supervisor.ExecFunc {
	return func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		return p.Exec(ctx, id, spec)
	}
}

// Processes reads the table shard-init keeps in the exit file, which a stop leaves as the last run had it.
func (p *Provider) Processes(_ context.Context, id string) ([]models.ProcessReport, error) {
	b, err := p.open(id)
	if err != nil {
		return nil, err
	}

	return bundle.ReadProcessTable(b.ExitFile)
}

// ProcessLogPath names the log on the sandbox's disk, which a stop unmounts, so a read after one mounts it again.
func (p *Provider) ProcessLogPath(id, name string) (string, error) {
	b, err := p.open(id)
	if err != nil {
		return "", err
	}
	path, err := b.ProcessLog(name)
	if err != nil {
		return "", fmt.Errorf("sandbox %s: %w", id, err)
	}
	if err := b.KeepDisk(); err != nil {
		return "", fmt.Errorf("sandbox %s: mount the disk its logs are on: %w", id, err)
	}

	return path, nil
}
