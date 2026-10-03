package broker

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// SHARD-343: one record that will not decode must not deny every other sandbox its proxy decision.
func TestDecideJudgesTheReadableSandboxesDespiteAnUnreadableRecord(t *testing.T) {
	records := fakeRecords{
		sandboxes: []models.Sandbox{{ID: "locked", Policy: "web", Address: netip.MustParsePrefix("10.87.0.2/16")}},
		err:       &sandboxstate.UnreadableError{ID: "broken", Err: errors.New("decode sandbox.json: unexpected end of JSON input")},
	}
	web := models.Policy{Name: "web", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{80, 443}},
	}}
	b := newBroker(t, records, fakeSecrets{}, web)

	got, err := b.Decide(t.Context(), proxy.Request{Source: source, Host: "api.example.com", Port: 443, TLS: true})
	if err != nil {
		t.Fatalf("Decide errored while one record was unreadable: %v", err)
	}
	if !got.Allowed {
		t.Errorf("Decide = %+v, want the readable sandbox judged", got)
	}
}
