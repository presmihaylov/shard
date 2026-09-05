package sandbox_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// An unclaimed verb is refused before any lock or claim, and the refusal names the provider and the verb.
func TestAnUnclaimedVerbIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	cases := []struct {
		verb     string
		sb       models.Sandbox
		call     func(*sandbox.Service) error
		withhold func(*fakeProvider)
	}{
		{models.VerbPause, running(),
			func(svc *sandbox.Service) error { _, err := svc.Pause(t.Context(), "sandbox1"); return err },
			func(p *fakeProvider) { p.noPause = true }},
		{models.VerbResume, pausedSandbox(),
			func(svc *sandbox.Service) error { _, err := svc.Resume(t.Context(), "sandbox1"); return err },
			func(p *fakeProvider) { p.noResume = true }},
		{models.VerbFork, pausedSandbox(),
			func(svc *sandbox.Service) error {
				_, err := svc.Fork(t.Context(), "sandbox1", sandbox.CopyRequest{})
				return err
			},
			func(p *fakeProvider) { p.noFork = true }},
	}

	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, tc.sb)
			tc.withhold(l.provider)

			err := tc.call(svc)
			if !errors.Is(err, models.ErrUnsupported) {
				t.Fatalf("%s on a provider without it returned %v, want ErrUnsupported", tc.verb, err)
			}
			if !strings.Contains(err.Error(), "fake") || !strings.Contains(err.Error(), tc.verb) {
				t.Errorf("the refusal reads %q, want the provider and the verb in it", err)
			}
			// The recorder sees every read, claim and provider call, so an empty list is the proof.
			if len(r.calls) != 0 {
				t.Errorf("the refusal came after %v", r.calls)
			}
		})
	}
}
