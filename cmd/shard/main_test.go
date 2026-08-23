package main

import (
	"os"
	"testing"
	"time"
)

// settle bounds the wait for something that must not happen. It is the whole assertion, so it is
// long enough that a loaded machine does not read as a pass.
const settle = 200 * time.Millisecond

// The first stop signal belongs to the work, which cancels and gives its claims back. Only the
// second one belongs to the process.
func TestTheFirstStopSignalCancelsAndTheSecondLeaves(t *testing.T) {
	signals := make(chan os.Signal, stopSignals)
	cancelled, left := make(chan struct{}), make(chan struct{})

	go escape(signals, func() { close(cancelled) }, func() { close(left) })

	signals <- os.Interrupt
	<-cancelled

	select {
	case <-left:
		t.Fatal("the first stop signal left the process, so a give-back never runs")
	case <-time.After(settle):
	}

	signals <- os.Interrupt
	<-left
}

// The second signal can arrive while the first is still being handled. A channel registered only
// after the cancellation would never see it, and the user then needed a third to leave.
func TestNoStopSignalIsLostBehindTheCancellation(t *testing.T) {
	signals := make(chan os.Signal, stopSignals)
	signals <- os.Interrupt
	signals <- os.Interrupt

	left := make(chan struct{})
	go escape(signals, func() {}, func() { close(left) })

	select {
	case <-left:
	case <-time.After(settle):
		t.Fatal("a second stop signal that arrived during the cancellation was dropped")
	}
}
