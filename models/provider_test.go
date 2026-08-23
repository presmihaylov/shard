package models_test

import (
	"errors"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// Only Stop takes a sandbox out of Alive, which is the whole keep-alive default.
func TestStatusIsAliveInEveryStateButStopped(t *testing.T) {
	cases := map[models.State]bool{
		models.StateCreated: true,
		models.StateRunning: true,
		models.StatePaused:  true,
		models.StateStopped: false,
	}

	for state, want := range cases {
		if got := (models.Status{Exists: true, State: state}).Alive(); got != want {
			t.Errorf("a sandbox that is %s reports alive=%v, want %v", state, got, want)
		}
	}

	if (models.Status{State: models.StateRunning}).Alive() {
		t.Error("a sandbox the substrate does not hold reports alive")
	}
}

// Refuse, never downgrade: a refusal must name the provider and the verb, not only match a sentinel.
func TestUnsupportedNamesTheProviderAndTheVerb(t *testing.T) {
	err := models.Unsupported("gvisor", "fork")

	if !errors.Is(err, models.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}

	var refusal *models.UnsupportedError
	if !errors.As(err, &refusal) {
		t.Fatalf("got %v, want an UnsupportedError", err)
	}
	if refusal.Provider != "gvisor" || refusal.Verb != "fork" {
		t.Errorf("got provider %q verb %q, want gvisor and fork", refusal.Provider, refusal.Verb)
	}
}
