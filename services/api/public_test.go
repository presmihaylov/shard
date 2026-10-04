package api_test

import (
	"net/http"
	"net/netip"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
)

// hostKeys name the host side of a record, which only the socket may answer.
var hostKeys = []string{"pid", "netns_path", "host_interface", "address", "checkpoint", "pausing", "exit_channel", "unresponsive_reason"}

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
		"GET /v0/local/sandboxes",
		"GET /v0/local/sandboxes/{id}",
	}
	slices.Sort(local)
	slices.Sort(want)
	if !slices.Equal(local, want) {
		t.Errorf("the local routes are %v, want %v", local, want)
	}
}

// hostSide is a running sandbox with every host field set, so a public answer that drops one proves it.
func hostSide(t *testing.T, s seeded) models.Sandbox {
	t.Helper()

	sb, err := s.repo.Create(models.Sandbox{
		Image:              "docker.io/library/alpine:3.20",
		Provider:           "gvisor",
		State:              models.StateRunning,
		ExitChannel:        "the exit pipe closed",
		UnresponsiveReason: "the guest missed two pings",
		Checkpoint:         "/var/lib/shard/checkpoints/sb",
		Pausing:            true,
		PID:                4242,
		NetnsPath:          "/run/netns/shard-sb",
		Address:            netip.MustParsePrefix("10.88.0.9/24"),
		HostInterface:      "shv-sb",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return sb
}

func TestThePublicRecordLeavesOutTheHostSideAndTheLocalOneKeepsIt(t *testing.T) {
	s := seed(t)
	sb := hostSide(t, s)

	_, list := get(t, s.server, "/v0/sandboxes")
	_, one := get(t, s.server, "/v0/sandboxes/"+sb.ID)
	for path, record := range map[string]map[string]any{"/v0/sandboxes": rowOf(t, list, sb.ID), "/v0/sandboxes/{id}": one} {
		if record["id"] != sb.ID {
			t.Fatalf("GET %s answered %v, want the record of %s", path, record, sb.ID)
		}
		checkNoHostKeys(t, "GET "+path, record)
	}

	_, localList := get(t, s.server, "/v0/local/sandboxes")
	_, localOne := get(t, s.server, "/v0/local/sandboxes/"+sb.ID)
	for path, record := range map[string]map[string]any{"/v0/local/sandboxes": rowOf(t, localList, sb.ID), "/v0/local/sandboxes/{id}": localOne} {
		checkHostKeys(t, "GET "+path, record)
	}
}

// checkNoHostKeys fails for each host field the record carries.
func checkNoHostKeys(t *testing.T, label string, record map[string]any) {
	t.Helper()

	for _, key := range hostKeys {
		if _, ok := record[key]; ok {
			t.Errorf("%s answered %s, a host field", label, key)
		}
	}
}

// checkHostKeys fails for each host field the record leaves out.
func checkHostKeys(t *testing.T, label string, record map[string]any) {
	t.Helper()

	for _, key := range hostKeys {
		if _, ok := record[key]; !ok {
			t.Errorf("%s left out %s, which the socket answers", label, key)
		}
	}
}

// rowOf is the row of id in a list body.
func rowOf(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()

	rows, _ := body["sandboxes"].([]any)
	for _, row := range rows {
		if record, ok := row.(map[string]any); ok && record["id"] == id {
			return record
		}
	}
	t.Fatalf("the list %v holds no row for %s", body, id)

	return nil
}

// A write verb answers the public record too, so a token that may start a sandbox never reads its pid.
func TestAWriteVerbAnswersThePublicRecord(t *testing.T) {
	s := seed(t)

	status, body := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/start", "")
	if status != http.StatusOK || body["state"] != string(models.StateRunning) {
		t.Fatalf("POST start answered %d %v", status, body)
	}
	checkNoHostKeys(t, "POST start", body)
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

	for path, want := range map[string]string{
		"/v0/sandboxes/" + sb.ID:       egress.GroupDNS,
		"/v0/local/sandboxes/" + sb.ID: "10.87.0.1",
	} {
		_, body := get(t, s.server, path)
		checkImpliedRules(t, path, body, want)
	}
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
func TestTheCreateStreamLeavesOutThePathAndTheHostSide(t *testing.T) {
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
	sb, _ := lines[1]["sandbox"].(map[string]any)
	checkNoHostKeys(t, "the streamed record", sb)
}
