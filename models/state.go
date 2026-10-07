package models

import "slices"

// State is the lifecycle state of a sandbox.
type State string

const (
	// StatePending is a create still pulling and starting in the background; it is where every create begins.
	StatePending State = "pending"
	StateCreated State = "created"
	StateRunning State = "running"
	// StatePaused holds a checkpoint on disk and no memory.
	StatePaused State = "paused"
	// StateUnresponsive is a running sandbox whose substrate process missed its probe bound; an answer makes it running again, and only stop ends it.
	StateUnresponsive State = "unresponsive"
	// StateStopped keeps the writable layer for a start; a sandbox stopped before its first start leaves nothing, since that stop is a delete.
	StateStopped State = "stopped"
	// StateFailed is a create or fork that never reached running, or a pause that lost the guest. It is terminal, so only rm frees it.
	StateFailed State = "failed"
)

// The whole machine, as https://useshards.com/docs/concepts/lifecycle/#the-moves lists it. stopped is not terminal here; failed is.
var legalTransitions = map[State][]State{
	StatePending:      {StateRunning, StateFailed},
	StateCreated:      {StateRunning, StateStopped, StateFailed},
	StateRunning:      {StatePaused, StateStopped, StateUnresponsive, StateFailed},
	StatePaused:       {StateRunning, StateStopped},
	StateUnresponsive: {StateRunning, StatePaused, StateStopped},
	StateStopped:      {StateRunning},
	StateFailed:       {},
}

// Valid reports whether s is a known state. A record on disk may predate it.
func (s State) Valid() bool {
	_, ok := legalTransitions[s]
	return ok
}

// CanTransitionTo reports whether the move is in the machine, not whether it is possible now.
func (s State) CanTransitionTo(next State) bool {
	return slices.Contains(legalTransitions[s], next)
}

// Live reports a state whose substrate process still runs, answering or not.
func (s State) Live() bool { return s == StateRunning || s == StateUnresponsive }
