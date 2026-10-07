package cli

import (
	"context"
	"os"
)

// InterruptedExitCode is what a shell reports for a command a SIGINT ended.
const InterruptedExitCode = 130

// Interrupts hands out main's stop signals: the first cancels the work, the second leaves.
type Interrupts struct {
	signals <-chan os.Signal
	cancel  context.CancelFunc
	leave   func()
}

// NewInterrupts reads signals, which main registers before any arrives, so none races the cancellation.
func NewInterrupts(signals <-chan os.Signal, cancel context.CancelFunc, leave func()) *Interrupts {
	return &Interrupts{signals: signals, cancel: cancel, leave: leave}
}

// Watch gives the second stop signal to the process, so a give-back that hangs cannot trap the user at the keyboard.
func (i *Interrupts) Watch() {
	cancelled := false
	for range i.signals {
		if cancelled {
			i.leave()

			return
		}

		cancelled = true
		i.cancel()
	}
}
