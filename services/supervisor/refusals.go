package supervisor

import (
	"errors"
	"log"
	"sync"
	"time"
)

const (
	// refusalWindow is how long the refusals after a logged one add to a count before one line names it.
	refusalWindow = time.Minute
	// redialFloor and redialCap bound the wait before a redial after a refusal, so a guest that floods its stream gets a few dials a second at most (SHARD-408).
	redialFloor = 100 * time.Millisecond
	redialCap   = 2 * time.Second
)

// Refusals logs the control lines one guest sends past MaxPayload or past the event queue: the first at once, the rest as one count a window (SHARD-390); it paces the redials after them too.
type Refusals struct {
	log    *log.Logger
	id     string
	window time.Duration

	mu sync.Mutex
	// open says a line went out in this window, so a refusal adds to held instead of logging.
	open bool
	held int
	last error
	// wait held off the last redial; only a new window starts it at the floor again, as a clean line per stream would cost a flood nothing.
	wait time.Duration
}

// NewRefusals logs the refusals of the sandbox id on logger.
func NewRefusals(logger *log.Logger, id string) *Refusals {
	return &Refusals{log: logger, id: id, window: refusalWindow}
}

// Note logs err when it is a refusal, and returns how long to wait before the redial: twice the last wait, up to the cap. Any other stream error is a reset the reconnect answers at once, quiet as before.
func (r *Refusals) Note(err error) time.Duration {
	if !errors.Is(err, ErrMessageTooLong) && !errors.Is(err, ErrEventFlood) {
		return 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = err
	if r.open {
		r.held++
		r.wait = min(2*r.wait, redialCap)

		return r.wait
	}
	r.open = true
	r.wait = redialFloor
	r.log.Printf("sandbox %s: refused a control message from the guest: %v; the stream reconnects", r.id, err)
	time.AfterFunc(r.window, r.tally)

	return r.wait
}

// tally names the refusals the window held, and closes the window when it held none.
func (r *Refusals) tally() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held == 0 {
		r.open = false

		return
	}
	r.log.Printf("sandbox %s: refused %d more control messages from the guest in the last %s; the latest: %v", r.id, r.held, r.window, r.last)
	r.held = 0
	time.AfterFunc(r.window, r.tally)
}
