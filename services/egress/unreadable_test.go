package egress

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// partialRecords lists its readable sandboxes and reports the rest unreadable, the way the real repository does.
type partialRecords struct {
	sandboxes  []models.Sandbox
	unreadable *sandboxstate.UnreadableError
}

func (p partialRecords) List() ([]models.Sandbox, error) { return p.sandboxes, p.unreadable }

// SHARD-343: one record that will not decode must not drop every other sandbox's chain.
func TestChainsSkipAnUnreadableRecord(t *testing.T) {
	s := newStore(t)
	if err := s.Set(models.Policy{Name: "web", Rules: []models.Rule{mustRule(t, models.ActionDeny, "any")}}); err != nil {
		t.Fatal(err)
	}

	records := partialRecords{
		sandboxes:  []models.Sandbox{{ID: "sandbox1", Policy: "web", State: models.StateRunning, Address: netip.MustParsePrefix("10.87.0.2/16")}},
		unreadable: &sandboxstate.UnreadableError{ID: "broken", Err: errors.New("decode sandbox.json: unexpected end of JSON input")},
	}

	chains, err := New(s, records, gateway, nameservers, fakeResolver{}).Chains(t.Context())
	if err != nil {
		t.Fatalf("Chains errored while one record was unreadable: %v", err)
	}
	if len(chains) != 1 || chains[0].Address != netip.MustParseAddr("10.87.0.2") {
		t.Fatalf("Chains = %+v, want the one readable sandbox's chain", chains)
	}
}
