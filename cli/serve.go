package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/services/daemon"
)

// serve runs the daemon: a resident peer over the same stores and locks as every other verb. It owns
// only background work, never the sandbox lifecycle, and systemd starts it, never another verb.
func (a App) serve(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("serve takes no arguments, got %d", len(args))
	}

	return daemon.New(a.Root, a.Out).Run(ctx)
}
