package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// seeded is a repository on disk with one running and one stopped sandbox, the way a daemon finds it.
type seeded struct {
	root     string
	repo     *sandboxstate.Repository
	policies *egress.Store
	running  models.Sandbox
	stopped  models.Sandbox
	verbs    *fakeLifecycle
	server   *httptest.Server
}

func seed(t *testing.T) seeded {
	t.Helper()

	root := t.TempDir()

	repo, err := sandboxstate.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	running := create(t, repo, "web", models.StateRunning)
	stopped := create(t, repo, "", models.StateStopped)

	policies, err := egress.NewStore(filepath.Join(root, "policies"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	secrets, err := secret.New(filepath.Join(root, "secrets"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	enforcer := egress.New(policies, repo, secrets, network.DefaultNameservers, nil)

	verbs := &fakeLifecycle{}

	server := httptest.NewServer(api.NewHandler("v-test", repo, enforcer, verbs, io.Discard))
	t.Cleanup(server.Close)

	return seeded{root: root, repo: repo, policies: policies, running: running, stopped: stopped, verbs: verbs, server: server}
}

func create(t *testing.T, repo *sandboxstate.Repository, name string, state models.State) models.Sandbox {
	t.Helper()

	sb, err := repo.Create(models.Sandbox{
		Name:      name,
		Image:     "docker.io/library/alpine:3.20",
		Provider:  "gvisor",
		State:     state,
		Address:   netip.MustParsePrefix("10.88.0.7/24"),
		CreatedAt: time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return sb
}

// get answers with the status and the decoded body, which is JSON on every route.
func get(t *testing.T, server *httptest.Server, path string) (int, map[string]any) {
	t.Helper()

	resp, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s answered Content-Type %q, want application/json", path, ct)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decode the body: %v", path, err)
	}

	return resp.StatusCode, body
}

func ids(t *testing.T, body map[string]any) []string {
	t.Helper()

	rows, ok := body["sandboxes"].([]any)
	if !ok {
		t.Fatalf("the body holds no sandboxes array: %v", body)
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		sb, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("a row is not an object: %v", row)
		}
		id, ok := sb["id"].(string)
		if !ok {
			t.Fatalf("a row carries no id: %v", sb)
		}
		out = append(out, id)
	}

	return out
}

func TestVersionIsWhatTheCLIPrints(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/version")
	if status != http.StatusOK || body["version"] != "v-test" {
		t.Errorf("GET /v0/version answered %d %v, want 200 and v-test", status, body)
	}
}

func TestListHidesTheStoppedSandboxesUnlessAll(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes")
	if status != http.StatusOK {
		t.Fatalf("GET /v0/sandboxes answered %d %v", status, body)
	}
	if got := ids(t, body); len(got) != 1 || got[0] != s.running.ID {
		t.Errorf("the list holds %v, want only the running sandbox %s", got, s.running.ID)
	}
	if _, ok := body["warnings"]; ok {
		t.Errorf("the list carries warnings with every record readable: %v", body["warnings"])
	}

	status, body = get(t, s.server, "/v0/sandboxes?all=true")
	if status != http.StatusOK {
		t.Fatalf("GET /v0/sandboxes?all=true answered %d %v", status, body)
	}
	if got := ids(t, body); len(got) != 2 {
		t.Errorf("the list with all holds %v, want both sandboxes", got)
	}
}

func TestListRefusesAnAllThatIsNotABoolean(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes?all=yes")
	if status != http.StatusBadRequest || !strings.Contains(body["error"].(string), "all=") {
		t.Errorf("GET /v0/sandboxes?all=yes answered %d %v, want 400 naming the query", status, body)
	}
}

// An unreadable record must not hide the others: the sandbox behind each one still holds a process.
func TestListAnswersTheReadableRowsAndWarnsAboutTheRest(t *testing.T) {
	s := seed(t)

	broken := create(t, s.repo, "", models.StateRunning)
	record := filepath.Join(s.root, "sandboxes", broken.ID, "sandbox.json")
	if err := os.WriteFile(record, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	status, body := get(t, s.server, "/v0/sandboxes")
	if status != http.StatusOK {
		t.Fatalf("GET /v0/sandboxes answered %d %v, want 200 with the readable rows", status, body)
	}
	if got := ids(t, body); len(got) != 1 || got[0] != s.running.ID {
		t.Errorf("the list holds %v, want the running sandbox that reads", got)
	}

	warnings, ok := body["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("the warnings are %v, want one line for the corrupt record", body["warnings"])
	}
	if !strings.Contains(warnings[0].(string), broken.ID) {
		t.Errorf("the warning %q does not name the corrupt sandbox %s", warnings[0], broken.ID)
	}
}

func TestListFailsWhenTheTreeCannotBeRead(t *testing.T) {
	s := seed(t)

	// A sandboxes directory that is gone is not a partial read: there are no rows to answer with.
	if err := os.RemoveAll(filepath.Join(s.root, "sandboxes")); err != nil {
		t.Fatalf("remove the tree: %v", err)
	}

	status, body := get(t, s.server, "/v0/sandboxes")
	if status != http.StatusInternalServerError || body["error"] == nil {
		t.Errorf("GET /v0/sandboxes answered %d %v, want 500 with an error", status, body)
	}
}

func TestGetAnswersForAnIDAndForAName(t *testing.T) {
	s := seed(t)

	for _, ref := range []string{s.running.ID, "web"} {
		status, body := get(t, s.server, "/v0/sandboxes/"+ref)
		if status != http.StatusOK || body["id"] != s.running.ID {
			t.Errorf("GET /v0/sandboxes/%s answered %d %v, want the record of %s", ref, status, body, s.running.ID)
		}
	}
}

// The record only names its policy; what the host enforces for it is compiled on the daemon, never on the client.
func TestGetCarriesWhatTheHostEnforces(t *testing.T) {
	s := seed(t)

	if err := s.policies.Set(models.Policy{Name: "deny-all", Rules: []models.Rule{
		{Action: models.ActionDeny, Destination: models.Destination{Kind: models.DestinationGroup, Value: "any"}},
	}}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	sb, err := s.repo.Create(models.Sandbox{Image: "docker.io/library/alpine:3.20", Provider: "gvisor", State: models.StateRunning, Policy: "deny-all"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	status, body := get(t, s.server, "/v0/sandboxes/"+sb.ID)
	if status != http.StatusOK {
		t.Fatalf("GET /v0/sandboxes/%s answered %d %v", sb.ID, status, body)
	}

	enforced, ok := body["egress"].(map[string]any)
	if !ok || enforced["policy"] != "deny-all" {
		t.Errorf("the record carries the egress %v, want the policy deny-all", body["egress"])
	}
	if rules, ok := enforced["rules"].([]any); !ok || len(rules) != 1 {
		t.Errorf("the egress holds the rules %v, want the one deny", enforced["rules"])
	}

	if _, body := get(t, s.server, "/v0/sandboxes/"+s.running.ID); body["egress"] != nil {
		t.Errorf("a sandbox with no policy carries the egress %v", body["egress"])
	}
}

func TestGetIs404WhenNothingHasTheReference(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes/ghost")
	if status != http.StatusNotFound || !strings.Contains(body["error"].(string), "ghost") {
		t.Errorf("GET /v0/sandboxes/ghost answered %d %v, want 404 naming ghost", status, body)
	}
}

func TestGetIs400WhenTheReferenceDoesNotValidate(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes/"+strings.Repeat("a", 65))
	if status != http.StatusBadRequest || !strings.Contains(body["error"].(string), "longer than") {
		t.Errorf("a 65 character reference answered %d %v, want 400", status, body)
	}
}

func TestGetIs500WhenTheNameLinkIsBroken(t *testing.T) {
	s := seed(t)

	// A link at something that cannot be an id is the host's state gone wrong, never a 404 or a 400.
	if err := os.Symlink("../sandboxes/not an id", filepath.Join(s.root, "names", "broken")); err != nil {
		t.Fatalf("plant the link: %v", err)
	}

	status, body := get(t, s.server, "/v0/sandboxes/broken")
	if status != http.StatusInternalServerError || !strings.Contains(body["error"].(string), "not a sandbox id") {
		t.Errorf("GET /v0/sandboxes/broken answered %d %v, want 500", status, body)
	}
}

func TestGetIs500WhenTheRecordIsUnreadable(t *testing.T) {
	s := seed(t)

	record := filepath.Join(s.root, "sandboxes", s.running.ID, "sandbox.json")
	if err := os.WriteFile(record, []byte("{not json"), 0o640); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	status, body := get(t, s.server, "/v0/sandboxes/"+s.running.ID)
	if status != http.StatusInternalServerError || !strings.Contains(body["error"].(string), "decode") {
		t.Errorf("GET of a corrupt record answered %d %v, want 500", status, body)
	}
}

func TestAnUnknownRouteIsAJSON404(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v1/nothing")
	if status != http.StatusNotFound || !strings.Contains(body["error"].(string), "/v1/nothing") {
		t.Errorf("GET /v1/nothing answered %d %v, want a JSON 404", status, body)
	}
}
