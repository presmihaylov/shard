package api_test

import (
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
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

// The spec types egress.rules as an array, so a policy with no rules and one the store no longer holds both answer [], never null.
func TestThePublicEgressOfAnEmptyOrMissingPolicyIsAnEmptyList(t *testing.T) {
	s := seed(t)

	if err := s.policies.Set(models.Policy{Name: "empty"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	for _, policy := range []string{"empty", "gone"} {
		sb, err := s.repo.Create(models.Sandbox{Image: "docker.io/library/alpine:3.20", Provider: "gvisor", State: models.StateRunning, Policy: policy})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		status, body := get(t, s.server, "/v0/sandboxes/"+sb.ID)
		enforced, _ := body["egress"].(map[string]any)
		if rules, ok := enforced["rules"].([]any); status != http.StatusOK || !ok || len(rules) != 0 {
			t.Errorf("GET the sandbox under the policy %s answered %d with the egress %v, want rules []", policy, status, body["egress"])
		}
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

// Every provider answers the same nine keys, and sysbox claims only the port.
func TestCapabilitiesAnswerEveryLifecycleVerb(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/capabilities")
	want := map[string]any{"create": true, "start": true, "stop": true, "remove": true, "pause": false, "resume": false, "fork": false, "snapshot": true, "port": true}
	if status != http.StatusOK || !maps.Equal(body, want) {
		t.Errorf("GET /v0/capabilities answered %d %v, want %v", status, body, want)
	}
}

// Discovery answers the table mint checks against, name and description, so the two never disagree.
func TestScopesAnswerTheTableMintChecks(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/scopes")
	listed, ok := body["scopes"].([]any)
	if status != http.StatusOK || len(body) != 1 || !ok || len(listed) != len(models.Scopes) {
		t.Fatalf("GET /v0/scopes answered %d %v, want only the %d scopes", status, body, len(models.Scopes))
	}
	for i, want := range models.Scopes {
		got, _ := listed[i].(map[string]any)
		if len(got) != 2 || got["name"] != want.Name || got["description"] != want.Description {
			t.Errorf("scope %d reads %v, want name %q and description %q", i, got, want.Name, want.Description)
		}
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

func TestPublicReadsFilterStoppedReasons(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		raw      string
		want     string
	}{
		{name: "operator stop", provider: "gvisor"},
		{name: "out of memory", provider: "gvisor", raw: sandbox.OOMKilledReason, want: sandbox.OOMKilledReason},
		{name: "process died", provider: "runc", raw: sandbox.DiedReason, want: sandbox.DiedReason},
		{name: "process lost", provider: "sysbox", raw: sandbox.LostReason, want: sandbox.LostReason},
		{name: "firecracker lost state", provider: "firecracker",
			raw:  sandbox.SupervisorFailedReason + ": sandbox sb1 lost its lifecycle state: open /var/lib/shard/sandboxes/sb1/exit: is a directory",
			want: "the sandbox lost its lifecycle state; start it again"},
		{name: "lost state outside a supervisor failure", provider: "firecracker", raw: "sandbox sb1 lost its lifecycle state: " + failedCause,
			want: "the sandbox stopped; the daemon log has the cause"},
		{name: "Mac VM supervisor failed", provider: "vz", raw: sandbox.SupervisorFailedReason + ": write /run/shard/exit: permission denied",
			want: "the init process of the sandbox failed; remove it and create another sandbox"},
		{name: "supervisor failed without a cause", provider: "firecracker", raw: sandbox.SupervisorFailedReason,
			want: "the init process of the sandbox failed; remove it and create another sandbox"},
		{name: "unknown diagnosis", provider: "firecracker", raw: failedCause, want: "the sandbox stopped; the daemon log has the cause"},
		{name: "diagnosis after a known reason", provider: "firecracker", raw: sandbox.DiedReason + ": " + failedCause,
			want: "the sandbox stopped; the daemon log has the cause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := seed(t)
			sb, err := s.repo.Create(models.Sandbox{Name: "reason", Image: "docker.io/library/alpine:3.20", Provider: tc.provider,
				State: models.StateStopped, StoppedReason: tc.raw})
			if err != nil {
				t.Fatal(err)
			}

			for _, path := range []string{"/v0/sandboxes/" + sb.ID, "/v0/sandboxes/" + sb.ID + "?wait=true", "/v0/sandboxes?all=true"} {
				status, body := get(t, s.server, path)
				if status != http.StatusOK {
					t.Fatalf("GET %s answered %d %v, want 200", path, status, body)
				}

				if path == "/v0/sandboxes?all=true" {
					body = listedSandbox(t, body, sb.ID)
				}

				got, _ := body["stopped_reason"].(string)
				if body["id"] != sb.ID || got != tc.want {
					t.Errorf("GET %s answered %v, want sandbox %s with stopped_reason %q", path, body, sb.ID, tc.want)
				}
			}

			local, err := s.repo.Get(sb.ID)
			if err != nil {
				t.Fatal(err)
			}
			if local.StoppedReason != tc.raw {
				t.Errorf("local stopped_reason = %q, want %q", local.StoppedReason, tc.raw)
			}
		})
	}
}

func listedSandbox(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()

	rows, ok := body["sandboxes"].([]any)
	if !ok {
		t.Fatalf("the list answered %v, want a sandboxes array", body)
	}
	for _, row := range rows {
		listed, ok := row.(map[string]any)
		if ok && listed["id"] == id {
			return listed
		}
	}

	t.Fatalf("the list answered %v, want sandbox %s", body, id)

	return nil
}
