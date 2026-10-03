package daemon

import (
	"context"
	"log"

	"github.com/presmihaylov/shard/models"
)

// sandboxErrors logs each error a tick returns once while it lasts, so a sandbox that fails every tick neither stops its task nor floods the log.
type sandboxErrors struct {
	logger *log.Logger
	task   string
	last   map[string]bool
	// bySandbox is the error each sandbox last returned, for a task that hears each sandbox on its own and not one tick at a time.
	bySandbox map[string]string
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

// probe takes one sandbox's result, nil included, since a pass is what lets the same error log again later.
func (e *sandboxErrors) probe(ctx context.Context, id string, err error) {
	// A daemon on its way down cut the probe short, which says nothing about a sandbox.
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		delete(e.bySandbox, id)

		return
	}

	text := err.Error()
	if e.bySandbox[id] != text {
		e.logger.Printf("task %s: %s; the task goes on", e.task, text)
	}
	if e.bySandbox == nil {
		e.bySandbox = map[string]string{}
	}
	e.bySandbox[id] = text
}

// forget drops the error of each sandbox the task no longer probes, so a removed one holds nothing and a new run logs afresh.
func (e *sandboxErrors) forget(sandboxes []models.Sandbox, probed func(models.Sandbox) bool) {
	kept := map[string]bool{}
	for _, sb := range sandboxes {
		kept[sb.ID] = probed(sb)
	}
	for id := range e.bySandbox {
		if !kept[id] {
			delete(e.bySandbox, id)
		}
	}
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
