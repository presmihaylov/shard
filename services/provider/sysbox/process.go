package sysbox

import (
	"context"
	"fmt"
	"time"

	"github.com/presmihaylov/shard/models"
)

// StartProcess is not wired on this substrate yet.
func (p *Provider) StartProcess(_ context.Context, id string, _ models.ProcessSpec) error {
	return fmt.Errorf("sandbox %s: %w", id, models.Unsupported(p.Name(), "run"))
}

// StopProcess is not wired on this substrate yet.
func (p *Provider) StopProcess(_ context.Context, id, _ string, _ time.Duration) error {
	return fmt.Errorf("sandbox %s: %w", id, models.Unsupported(p.Name(), "kill"))
}

// Processes is not wired on this substrate yet.
func (p *Provider) Processes(context.Context, string) ([]models.ProcessReport, error) {
	return nil, nil
}

// ProcessLogPath is not wired on this substrate yet.
func (p *Provider) ProcessLogPath(id, _ string) (string, error) {
	return "", fmt.Errorf("sandbox %s: %w", id, models.Unsupported(p.Name(), "logs"))
}
