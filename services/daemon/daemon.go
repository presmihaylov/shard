// Package daemon runs the background work beside the CLI: the same stores and the same locks,
// never the sandbox lifecycle. No CLI verb requires it, and no CLI verb starts it.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
)

// LockFile is the singleton flock under the root: one daemon per root, and its liveness signal.
const LockFile = "daemon.lock"

const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = time.Minute
	// defaultHealthyAfter is how long a run must last for the next failure to start the backoff over.
	defaultHealthyAfter = time.Minute
)

// Daemon supervises the background tasks for one root.
type Daemon struct {
	root  string
	tasks []Task
	log   *log.Logger

	minBackoff   time.Duration
	maxBackoff   time.Duration
	healthyAfter time.Duration
}

// New builds a daemon over root that logs to out, which systemd turns into the journal.
func New(root string, out io.Writer, tasks ...Task) *Daemon {
	return &Daemon{
		root:         root,
		tasks:        tasks,
		log:          log.New(out, "", log.LstdFlags),
		minBackoff:   defaultMinBackoff,
		maxBackoff:   defaultMaxBackoff,
		healthyAfter: defaultHealthyAfter,
	}
}

// Run holds the singleton and supervises every task until ctx ends. A second daemon on the root is refused.
func (d *Daemon) Run(ctx context.Context) (err error) {
	path := filepath.Join(d.root, LockFile)

	lock, err := store.TryAcquire(path, 0o600)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another shard serve already holds %s", path)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	d.log.Printf("serve holds %s with %d tasks", path, len(d.tasks))

	var wg sync.WaitGroup
	for _, t := range d.tasks {
		wg.Go(func() { d.supervise(ctx, t) })
	}
	wg.Wait()
	<-ctx.Done()

	return nil
}

// Alive reports whether a daemon holds root right now. It is advisory: the holder may die a moment
// later, so a caller must still tolerate its absence.
func Alive(root string) (bool, error) {
	lock, err := store.TryAcquireShared(filepath.Join(root, LockFile), 0o600)
	if err != nil {
		return false, err
	}
	if lock == nil {
		return true, nil
	}

	return false, lock.Release()
}
