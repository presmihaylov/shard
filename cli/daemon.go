package cli

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/sandbox"
)

// daemon runs the resident process systemd starts: the background work, the API socket and the sandbox lifecycle.
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

	return api.Serve(ctx, listener, api.NewHandler(t.app.Version, repo, enforcer, &lifecycle{deps: d}, t.app.Out))
}

// lifecycle builds the orchestrator on the first verb, so a daemon on a host without runsc still answers reads.
type lifecycle struct {
	deps *deps

	mu  sync.Mutex
	svc *sandbox.Service
}

func (l *lifecycle) service() (*sandbox.Service, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.svc != nil {
		return l.svc, nil
	}

	svc, err := l.deps.lifecycle()
	if err != nil {
		return nil, err
	}
	l.svc = svc

	return l.svc, nil
}

func (l *lifecycle) Create(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Create(ctx, req)
}

func (l *lifecycle) Start(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Start(ctx, ref)
}

func (l *lifecycle) Stop(ctx context.Context, ref string, grace time.Duration) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Stop(ctx, ref, grace)
}

func (l *lifecycle) Remove(ctx context.Context, ref string, force bool, grace time.Duration) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.Remove(ctx, ref, force, grace)
}
