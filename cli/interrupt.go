package cli

import (
	"context"
	"os"
	"sync"
)

// InterruptedExitCode is what a shell reports for a command a SIGINT ended.
const InterruptedExitCode = 130

// Interrupts hands out main's stop signals: the first cancels the work, the second leaves, unless a verb such as run took them.
type Interrupts struct {
	signals <-chan os.Signal
	cancel  context.CancelFunc
	leave   func()

	mu    sync.Mutex
	taken chan os.Signal
}

// NewInterrupts reads signals, which main registers before any arrives, so none races the cancellation.
func NewInterrupts(signals <-chan os.Signal, cancel context.CancelFunc, leave func()) *Interrupts {
	return &Interrupts{signals: signals, cancel: cancel, leave: leave}
}

// Watch gives the second stop signal to the process, so a give-back that hangs cannot trap the user at the keyboard.
func (i *Interrupts) Watch() {
	cancelled := false
	for s := range i.signals {
		if taken := i.receiver(); taken != nil {
			taken <- s

			continue
		}
		if cancelled {
			i.leave()

			return
		}

		cancelled = true
		i.cancel()
	}
}

// take hands every later stop signal to the caller; an App with none, as a test builds, hands it nothing.
func (i *Interrupts) take() <-chan os.Signal {
	if i == nil {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.taken = make(chan os.Signal)

	return i.taken
}

func (i *Interrupts) receiver() chan os.Signal {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.taken
}
