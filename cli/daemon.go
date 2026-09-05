package cli

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/daemon"
)

// daemon runs the resident process systemd starts: the background work and the API socket, never the lifecycle.
func (a App) daemon(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("daemon takes no arguments, got %d", len(args))
	}

	return daemon.New(a.Root, a.Out, apiTask{app: a}).Run(ctx)
}

// apiTask serves the REST API on the socket under the root. The daemon restarts it when the listener dies.
type apiTask struct {
	app App
}

func (apiTask) Name() string { return "api" }

func (t apiTask) Run(ctx context.Context) error {
	d := t.app.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	enforcer, err := d.egress()
	if err != nil {
		return err
	}

	listener, mode, group, err := api.Listen(t.app.Root)
	if err != nil {
		return err
	}

	owner := "no " + api.Group + " group on this host"
	if group != "" {
		owner = "group " + group
	}
	log.New(t.app.Out, "", log.LstdFlags).Printf("api listening on %s, mode %04o, %s", filepath.Join(t.app.Root, api.SocketFile), mode, owner)

	return api.Serve(ctx, listener, api.NewHandler(t.app.Version, repo, enforcer, t.app.Out))
}
