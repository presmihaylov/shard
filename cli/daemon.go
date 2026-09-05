package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/services/daemon"
)

// daemon runs the resident process systemd starts: the background work, the API socket and the sandbox lifecycle.
func (a App) daemon(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("daemon takes no arguments, got %d", len(args))
	}

	return daemon.Run(ctx, daemon.Config{
		Version:     a.Version,
		Root:        a.Root,
		Out:         a.Out,
		Insecure:    a.Insecure,
		PullTimeout: a.Timeout,
		InitPath:    a.InitPath,
	})
}
