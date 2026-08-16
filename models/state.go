package models

import "slices"

// State is the lifecycle state of a sandbox.
//
// The set is deliberately small. There is no checkpointed state: pause writes the
// snapshot to disk and frees the memory, so a paused sandbox is the only paused
// thing there is.
type State string

const (
	// StateCreated means the sandbox exists on disk but no process runs yet.
	StateCreated State = "created"
	// StateRunning means the entrypoint is running.
	StateRunning State = "running"
	// StatePaused means a snapshot exists on disk and no memory is held.
	StatePaused State = "paused"
	// StateStopped means no process runs and no snapshot exists. The writable
	// layer survives, which is what makes StateStopped -> StateRunning legal.
	StateStopped State = "stopped"
)

// legalTransitions is the whole state machine. docs/state-machine.md draws it.
//
// StateStopped is NOT terminal, even though it is terminal at the runsc level:
// shard start re-runs the entrypoint over the preserved writable layer.
var legalTransitions = map[State][]State{
	StateCreated: {StateRunning, StateStopped},
	StateRunning: {StatePaused, StateStopped},
	StatePaused:  {StateRunning, StateStopped},
	StateStopped: {StateRunning},
}

// Valid reports whether s is one of the four known states. A state record read
// from disk was written by some other version of shard, so it needs checking.
func (s State) Valid() bool {
	_, ok := legalTransitions[s]
	return ok
}

// CanTransitionTo reports whether s -> next is a legal move. It answers only
// whether the move is in the machine; whether it is possible right now is the
// orchestrator's question, and an unsupported verb is the provider's.
func (s State) CanTransitionTo(next State) bool {
	return slices.Contains(legalTransitions[s], next)
}
