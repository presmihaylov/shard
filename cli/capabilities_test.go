package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// An unclaimed verb is refused before any hold or claim, and the refusal names the provider and the verb.
func TestAnUnclaimedVerbIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	cases := []struct {
		verb     string
		args     []string
		sb       models.Sandbox
		withhold func(*fakeLifecycleProvider)
	}{
		{models.VerbPause, []string{"pause", "sandbox1"}, running(), func(p *fakeLifecycleProvider) { p.noPause = true }},
		{models.VerbResume, []string{"resume", "sandbox1"}, paused(), func(p *fakeLifecycleProvider) { p.noResume = true }},
		{models.VerbFork, []string{"fork", "sandbox1"}, paused(), func(p *fakeLifecycleProvider) { p.noFork = true }},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			r := &recorder{}
			app, d := newLifecycleApp(t, &bytes.Buffer{}, r, tc.sb)
			tc.withhold(d.providerSvc.(*fakeLifecycleProvider))

			err := app.Run(t.Context(), tc.args)
			if !errors.Is(err, models.ErrUnsupported) {
				t.Fatalf("%s on a provider without it returned %v, want ErrUnsupported", tc.verb, err)
			}
			if !strings.Contains(err.Error(), "fake") || !strings.Contains(err.Error(), tc.verb) {
				t.Errorf("the refusal reads %q, want the provider and the verb in it", err)
			}
			// The recorder sees every hold, read, claim and provider call, so an empty list is the proof.
			if len(r.calls) != 0 {
				t.Errorf("the refusal came after %v", r.calls)
			}
		})
	}
}
