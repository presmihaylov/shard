package models_test

import (
	"testing"

	"github.com/presmihaylov/shard/models"
)

// Every state, so the table below is exhaustive rather than the moves someone remembered.
var allStates = []models.State{
	models.StateCreated,
	models.StateRunning,
	models.StatePaused,
	models.StateStopped,
}

func TestCanTransitionTo(t *testing.T) {
	legal := map[models.State]map[models.State]bool{
		models.StateCreated: {models.StateRunning: true, models.StateStopped: true},
		models.StateRunning: {models.StatePaused: true, models.StateStopped: true},
		models.StatePaused:  {models.StateRunning: true, models.StateStopped: true},
		models.StateStopped: {models.StateRunning: true},
	}

	for _, from := range allStates {
		for _, to := range allStates {
			want := legal[from][to]
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s: got %v, want %v", from, to, got, want)
			}
		}
	}
}

// The transition most likely to be deleted by someone who reads stopped as finished.
func TestStoppedIsNotTerminal(t *testing.T) {
	if !models.StateStopped.CanTransitionTo(models.StateRunning) {
		t.Error("stopped -> running must stay legal; shard start depends on it")
	}
}

func TestNoStateTransitionsToItself(t *testing.T) {
	for _, s := range allStates {
		if s.CanTransitionTo(s) {
			t.Errorf("%s -> %s: a state must not transition to itself", s, s)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range allStates {
		if !s.Valid() {
			t.Errorf("%s: want valid", s)
		}
	}

	for _, s := range []models.State{"", "checkpointed", "Running"} {
		if s.Valid() {
			t.Errorf("%q: want invalid", s)
		}
	}
}
