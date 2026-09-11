package models_test

import (
	"errors"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestNoSnapshotsRefusesEveryOptionalVerbByName(t *testing.T) {
	n := models.NoSnapshots{Provider: "sysbox"}

	if n.Capabilities() != (models.Capabilities{}) {
		t.Errorf("got capabilities %+v, want none", n.Capabilities())
	}

	cases := map[string]error{
		models.VerbPause:  n.Pause(t.Context(), "amber-otter-1a2b", "/snap"),
		models.VerbResume: n.Resume(t.Context(), "amber-otter-1a2b", "/snap"),
		models.VerbFork:   n.Fork(t.Context(), "/snap", models.SandboxSpec{}),
	}

	for verb, err := range cases {
		var refusal *models.UnsupportedError
		if !errors.As(err, &refusal) {
			t.Fatalf("%s returned %v, want an UnsupportedError", verb, err)
		}
		if refusal.Provider != "sysbox" || refusal.Verb != verb {
			t.Errorf("%s names provider %q verb %q, want sysbox and %s", verb, refusal.Provider, refusal.Verb, verb)
		}
	}
}
