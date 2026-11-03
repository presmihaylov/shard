package daemon

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// gate opens once, when the task that holds it is up.
type gate struct {
	once sync.Once
	ch   chan struct{}
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

// open is a no-op on a nil gate, which a task a test builds without one carries.
func (g *gate) open() {
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.ch) })
}

// autostart brings back, once per daemon start, the sandboxes and processes whose restart policy says a daemon start does.
type autostart struct {
	deps      *deps
	lifecycle *lifecycle
	// after are the proxy, the resolver and the host firewall, which a started guest reaches at once.
	after []*gate
	wait  time.Duration
}

// autostartWait bounds the wait on a network task that never comes up, which should cost a sandbox its start no more than this.
const autostartWait = 30 * time.Second

func (autostart) Name() string { return "autostart" }

func (t autostart) Run(ctx context.Context) error {
	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)
	if !t.ready(ctx) {
		if ctx.Err() != nil {
			return nil
		}
		logger.Printf("task %s: the proxy, the resolver or the host firewall is not up after %s; the sandboxes start without it", t.Name(), t.wait)
	}

	repo, err := t.deps.repo()
	if err != nil {
		return err
	}
	sandboxes, err := sandboxstate.ListReadable(repo, t.deps.unreadableLog())
	if err != nil {
		return err
	}
	// A root that owes no start needs no substrate, so a host without runsc keeps its daemon.
	if !slices.ContainsFunc(sandboxes, sandbox.Owed) {
		return nil
	}

	svc, err := t.lifecycle.service()
	if err != nil {
		return err
	}
	// SHARD-376 (shard's ruling): a sandbox's error is logged and the rest still start.
	failures := sandboxErrors{logger: logger, task: t.Name()}
	failures.tick(ctx, svc.Autostart(ctx, sandboxes, func(line string) { logger.Print(line) }))

	return nil
}

// ready waits for every gate, and says false when the wait ran out or ctx ended first.
func (t autostart) ready(ctx context.Context) bool {
	timeout := time.NewTimer(t.wait)
	defer timeout.Stop()

	for _, g := range t.after {
		select {
		case <-g.ch:
		case <-timeout.C:
			return false
		case <-ctx.Done():
			return false
		}
	}

	return true
}
