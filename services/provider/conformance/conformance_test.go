package conformance

import (
	"testing"

	"github.com/presmihaylov/shard/models"
)

// stub answers the one call check makes. The rest of the interface is never reached.
type stub struct{ models.Provider }

func (stub) Name() string { return "stub" }

// A refusal carries the lowercase verb, so a check against the Go method name would fail every
// optional subtest on a provider that supports none of them.
func TestCheckSkipsARefusalThatNamesTheProviderAndTheVerb(t *testing.T) {
	s := Subject{Provider: stub{}}

	for _, verb := range []string{models.VerbPause, models.VerbResume, models.VerbFork} {
		ok := t.Run(verb, func(t *testing.T) {
			s.check(t, verb, false, models.Unsupported("stub", verb))
		})
		if !ok {
			t.Errorf("check failed the %s subtest on a well-formed refusal", verb)
		}
	}
}
