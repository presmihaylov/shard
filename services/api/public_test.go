package api_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
)

// The front forwards public routes only, so a route with no class would be neither served remotely nor refused on purpose.
func TestEveryRouteIsPublicOrLocalAndTheLocalOnesAreTheHostOnes(t *testing.T) {
	var local []string
	for _, r := range api.Routes() {
		if r.Class != api.Public && r.Class != api.Local {
			t.Errorf("route %s %s has the class %q, want public or local", r.Method, r.Pattern, r.Class)
		}
		if r.Class == api.Local {
			local = append(local, r.Method+" "+r.Pattern)
		}
	}

	want := []string{
		"GET /v0/daemon",
		"GET /v0/images",
		"POST /v0/images/pull",
		"POST /v0/images/prune",
		"DELETE /v0/images/{ref...}",
	}
	slices.Sort(local)
	slices.Sort(want)
	if !slices.Equal(local, want) {
		t.Errorf("the local routes are %v, want %v", local, want)
	}
}

// The implied rules open DNS to the bridge gateway, so the public egress names the dns group and keeps each id.
func TestThePublicEgressNamesTheImpliedRulesAsTheDNSGroup(t *testing.T) {
	s := seed(t)

	if err := s.policies.Set(models.Policy{Name: "named", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "example.com"}, Protocol: "tcp", Ports: []int{443}},
	}}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	sb, err := s.repo.Create(models.Sandbox{Image: "docker.io/library/alpine:3.20", Provider: "gvisor", State: models.StateRunning, Policy: "named"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	path := "/v0/sandboxes/" + sb.ID
	_, body := get(t, s.server, path)
	checkImpliedRules(t, path, body, egress.GroupDNS)
}

// checkImpliedRules fails unless the record's egress leads with the two implied dns rules, ids 1 and 2, each naming want.
func checkImpliedRules(t *testing.T, path string, body map[string]any, want string) {
	t.Helper()

	enforced, _ := body["egress"].(map[string]any)
	rules, _ := enforced["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("GET %s carries the rules %v, want the two implied dns rules and the domain", path, rules)
	}
	for i, rule := range rules[:2] {
		r, _ := rule.(map[string]any)
		destination, _ := r["destination"].(map[string]any)
		if destination["value"] != want || r["id"] != []string{"1", "2"}[i] || r["implied"] != "dns" {
			t.Errorf("GET %s: implied rule %d reads %v, want id %d naming %s", path, i, r, i+1, want)
		}
	}
}

func TestVersionNamesTheAPIVersion(t *testing.T) {
	s := seed(t)

	if _, body := get(t, s.server, "/v0/version"); body["api_version"] != api.APIVersion {
		t.Errorf("GET /v0/version answered %v, want api_version %s", body, api.APIVersion)
	}
}

// A client reads an empty list as a provider that refuses nothing, so the list is never null.
func TestCapabilitiesNameEveryVerbTheProviderRefuses(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/capabilities")
	unsupported, ok := body["unsupported"].([]any)
	if status != http.StatusOK || body["provider"] != "sysbox" || !ok {
		t.Fatalf("GET /v0/capabilities answered %d %v, want sysbox and a list", status, body)
	}
	if got := []any{models.VerbPause, models.VerbResume, models.VerbFork}; !slices.Equal(unsupported, got) {
		t.Errorf("sysbox refuses %v, want %v", unsupported, got)
	}
}

// A create is public, so its pull progress never names the host path the image lands at.
func TestTheCreateStreamLeavesOutThePath(t *testing.T) {
	s := seed(t)
	s.verbs.createdID = s.running.ID
	s.verbs.pulled = []image.Event{{Status: image.StatusPulled, Reference: "docker.io/library/alpine:3.20", Path: "/var/lib/shard/images/alpine"}}

	_, _, lines := sendStreamed(t, s.server, "/v0/sandboxes?wait=true", `{"image":"alpine:3.20"}`)
	if len(lines) != 2 {
		t.Fatalf("the create answered %v, want the event and the record", lines)
	}
	if event, _ := lines[0]["event"].(map[string]any); event["path"] != nil || event["reference"] == nil {
		t.Errorf("the event reads %v, want the reference and no path", lines[0])
	}
}
