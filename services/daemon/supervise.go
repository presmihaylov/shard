package daemon

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Task is one unit of background work the daemon keeps running.
type Task interface {
	Name() string
	// Run does the work until ctx ends. An error asks for a restart; nil ends the task for good.
	Run(ctx context.Context) error
}

// supervise restarts the task with backoff until ctx ends or the task says it is done.
func (d *Daemon) supervise(ctx context.Context, t Task) {
	backoff := d.minBackoff
	for {
		started := time.Now()
		err := run(ctx, t)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			d.log.Printf("task %s is done", t.Name())

			return
		}

		// A run that held long enough earns a fresh backoff; a crash loop keeps the long one.
		if time.Since(started) >= d.healthyAfter {
			backoff = d.minBackoff
		}
		d.log.Printf("task %s failed, restart in %s: %v", t.Name(), backoff, err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, d.maxBackoff)
	}
}

// run contains a panic: a task that panics must not take the daemon and its siblings with it.
func run(ctx context.Context, t Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task %s panicked: %v", t.Name(), r)
		}
	}()

	return t.Run(ctx)
}

// jitter spreads restarts so tasks that failed together do not retry together.
func jitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1) // #nosec G404 -- retry spread needs no cryptographic randomness.
}
