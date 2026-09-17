package models

import "slices"

// State is the lifecycle state of a sandbox.
type State string

const (
	// StatePending is a create still pulling and starting in the background; it is where every create begins.
	StatePending State = "pending"
	StateCreated State = "created"
	StateRunning State = "running"
	// StatePaused holds a snapshot on disk and no memory.
	StatePaused State = "paused"
	// StateStopped keeps the writable layer, so a start can follow it. A sandbox stopped before its
	// entrypoint ran leaves nothing on the substrate, because stopping that one is a delete there.
	StateStopped State = "stopped"
	// StateFailed is a create that never reached running. It is terminal, so only rm frees it.
	StateFailed State = "failed"
)

// The whole machine, drawn in docs/state-machine.md. stopped is not terminal here; failed is.
var legalTransitions = map[State][]State{
	StatePending: {StateRunning, StateFailed},
	StateCreated: {StateRunning, StateStopped},
	StateRunning: {StatePaused, StateStopped},
	StatePaused:  {StateRunning, StateStopped},
	StateStopped: {StateRunning},
	StateFailed:  {},
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
