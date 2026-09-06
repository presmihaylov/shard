// Package daemon is the resident process: it owns the state under one root, wires every layer the
// verbs drive, and serves them over the API socket. Every CLI verb goes through it.
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

// LockFile is the singleton flock under the root: one daemon per root, and the only lock shard keeps.
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

	lock, err := takeLock(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	d.log.Printf("daemon holds %s with %d tasks", path, len(d.tasks))

	var wg sync.WaitGroup
	for _, t := range d.tasks {
		wg.Go(func() { d.supervise(ctx, t) })
	}
	wg.Wait()
	<-ctx.Done()

	return nil
}

// takeLock is the one lock left in shard: it keeps a second daemon off a root the first one owns.
func takeLock(path string) (*store.Lock, error) {
	lock, err := store.TryAcquire(path, 0o600)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		return nil, fmt.Errorf("another shard daemon already holds %s", path)
	}

	return lock, nil
}
