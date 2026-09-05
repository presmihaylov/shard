package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// Config is the wiring one resident daemon needs.
type Config struct {
	// Version is what the version route answers with.
	Version string
	Root    string
	Out     io.Writer
	// Insecure lists the registry hosts the daemon may reach over plaintext http. Every other host is https.
	Insecure []string
	// PullTimeout bounds one pull; zero is no bound.
	PullTimeout time.Duration
	// InitPath is the host path of the guest supervisor.
	InitPath string
}

// Run supervises the daemon's tasks over one root until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	d := &deps{cfg: cfg}
	life := &lifecycle{deps: d}

	return New(cfg.Root, cfg.Out, apiTask{deps: d, lifecycle: life}).WithReconciler(reconciler{deps: d, lifecycle: life}).Run(ctx)
}

// reconciler checks the records against the substrate at start. An empty root needs no provider, so a
// host without runsc still gets a daemon that answers the reads and the store verbs.
type reconciler struct {
	deps      *deps
	lifecycle *lifecycle
}

func (r reconciler) Reconcile(ctx context.Context, report func(string)) error {
	repo, err := r.deps.repo()
	if err != nil {
		return err
	}

	sandboxes, unreadable := repo.List()
	if unreadable != nil {
		// A record shard cannot read is one it cannot correct either, and refusing to start would fix none.
		report(fmt.Sprintf("some records cannot be read, so they are not checked: %v", unreadable))
	}
	if len(sandboxes) == 0 {
		return nil
	}

	svc, err := r.lifecycle.service()
	if err != nil {
		return err
	}

	return svc.ReconcileAll(ctx, sandboxes, report)
}

// apiTask serves the REST API on the socket under the root. The daemon restarts it when the listener dies.
type apiTask struct {
	deps      *deps
	lifecycle *lifecycle
}

func (apiTask) Name() string { return "api" }

func (t apiTask) Run(ctx context.Context) error {
	cfg := t.deps.cfg

	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	enforcer, err := t.deps.egress()
	if err != nil {
		return err
	}

	stores, err := t.deps.stores()
	if err != nil {
		return err
	}

	listener, mode, group, err := api.Listen(cfg.Root)
	if err != nil {
		return err
	}

	owner := "no " + api.Group + " group on this host"
	if group != "" {
		owner = "group " + group
	}
	log.New(cfg.Out, "", log.LstdFlags).Printf("api listening on %s, mode %04o, %s", filepath.Join(cfg.Root, api.SocketFile), mode, owner)

	handler := api.NewHandler(cfg.Version, repo, enforcer, t.lifecycle, stores, cfg.Out)

	return api.Serve(ctx, listener, handler)
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

func (l *lifecycle) Pause(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Pause(ctx, ref)
}

func (l *lifecycle) Resume(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Resume(ctx, ref)
}

func (l *lifecycle) Fork(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Fork(ctx, ref, req)
}

func (l *lifecycle) Clone(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Clone(ctx, ref, req)
}

func (l *lifecycle) Exec(ctx context.Context, ref string, req sandbox.ExecRequest, streams sandbox.Streams) (models.ExitStatus, error) {
	svc, err := l.service()
	if err != nil {
		return models.ExitStatus{}, err
	}

	return svc.Exec(ctx, ref, req, streams)
}

func (l *lifecycle) ResizeExec(ctx context.Context, ref, execID string, size sandbox.TerminalSize) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.ResizeExec(ctx, ref, execID, size)
}

func (l *lifecycle) Logs(ctx context.Context, ref string, follow bool, w io.Writer) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.Logs(ctx, ref, follow, w)
}
