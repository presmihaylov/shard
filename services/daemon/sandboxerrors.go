package daemon

import (
	"context"
	"log"
)

// sandboxErrors logs each error a tick returns once while it lasts, so a sandbox that fails every tick neither stops its task nor floods the log.
type sandboxErrors struct {
	logger *log.Logger
	task   string
	last   map[string]bool
}

// tick takes what one tick returned, nil included, since a clean tick is what lets the same error log again later.
func (e *sandboxErrors) tick(ctx context.Context, err error) {
	// A daemon on its way down cut the tick short, which says nothing about a sandbox.
	if ctx.Err() != nil {
		return
	}

	seen := map[string]bool{}
	for _, part := range parts(err) {
		text := part.Error()
		if !e.last[text] && !seen[text] {
			e.logger.Printf("task %s: %s; the task goes on", e.task, text)
		}
		seen[text] = true
	}
	e.last = seen
}

// parts takes the tick's own errors.Join apart, one error per sandbox; a join inside one sandbox's error stays under its wrapper.
func parts(err error) []error {
	if err == nil {
		return nil
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}

	return []error{err}
}
