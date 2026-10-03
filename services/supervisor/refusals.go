package supervisor

import (
	"errors"
	"log"
	"sync"
	"time"
)

// refusalWindow is how long the refusals after a logged one add to a count before one line names it.
const refusalWindow = time.Minute

// Refusals logs the control lines one guest sends past MaxPayload: the first at once, the rest as one count a window (SHARD-390).
type Refusals struct {
	log    *log.Logger
	id     string
	window time.Duration

	mu sync.Mutex
	// open says a line went out in this window, so a refusal adds to held instead of logging.
	open bool
	held int
}

// NewRefusals logs the refusals of the sandbox id on logger.
func NewRefusals(logger *log.Logger, id string) *Refusals {
	return &Refusals{log: logger, id: id, window: refusalWindow}
}

// Note logs err when it is a refusal. Any other stream error is a reset the reconnect answers, so it stays quiet as before.
func (r *Refusals) Note(err error) {
	if !errors.Is(err, ErrMessageTooLong) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open {
		r.held++

		return
	}
	r.open = true
	r.log.Printf("sandbox %s: refused a control message from the guest: %v; the stream reconnects", r.id, err)
	time.AfterFunc(r.window, r.tally)
}

// tally names the refusals the window held, and closes the window when it held none.
func (r *Refusals) tally() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held == 0 {
		r.open = false

		return
	}
	r.log.Printf("sandbox %s: refused %d more control messages from the guest in the last %s: %v", r.id, r.held, r.window, ErrMessageTooLong)
	r.held = 0
	time.AfterFunc(r.window, r.tally)
}
