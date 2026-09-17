package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// seeded is a repository on disk with one running and one stopped sandbox, the way a daemon finds it.
type seeded struct {
	root     string
	repo     *sandboxstate.Repository
	policies *egress.Store
	running  models.Sandbox
	stopped  models.Sandbox
	verbs    *fakeLifecycle
	stores   *fakeStores
	egress   *fakeEgressLog
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

	enforcer := egress.New(policies, repo, network.DefaultNameservers, nil)

	verbs, stores, egressLog := &fakeLifecycle{ended: make(chan struct{})}, &fakeStores{}, &fakeEgressLog{}

	server := httptest.NewServer(api.NewHandler("v-test", fakeProcess{}, repo, enforcer, verbs, stores, egressLog, io.Discard))
	t.Cleanup(server.Close)

	return seeded{root: root, repo: repo, policies: policies, running: running, stopped: stopped, verbs: verbs, stores: stores, egress: egressLog, server: server}
}

// fakeProcess is a daemon that says it runs sysbox, or one whose provider cannot be built.
type fakeProcess struct {
	err error
}

func (f fakeProcess) Daemon() (api.Daemon, error) {
	if f.err != nil {
		return api.Daemon{}, f.err
	}

	return api.Daemon{
		PID:       4123,
		StartedAt: time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC),
		Socket:    "/var/lib/shard/shard.sock",
		Provider:  "sysbox",
		Proxy:     api.Proxy{PlainPort: 30080, TLSPort: 30443},
	}, nil
}

// fakeEgressLog answers with one line per sandbox, so the handler is what the test exercises.
type fakeEgressLog struct {
	// holds keeps a follow open until its context ends, the way a live log does while the sandbox runs.
	holds bool
}

func (*fakeEgressLog) Read(sb models.Sandbox) ([]egress.Record, error) {
	return []egress.Record{{Source: egress.SourceProxy, Verdict: string(models.ActionAllow), Host: sb.Name}}, nil
}

// Follow hands over the same line and then ends as a removed sandbox does, so a test needs no clock.
func (f *fakeEgressLog) Follow(ctx context.Context, sb models.Sandbox, yield func(egress.Record) error) error {
	records, err := f.Read(sb)
	if err != nil {
		return err
	}

	for _, record := range records {
		if err := yield(record); err != nil {
			return err
		}
	}

	if f.holds {
		<-ctx.Done()

		return ctx.Err()
	}

	return egress.ErrSandboxGone
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

// refused is the one object an error body carries.
type refused struct {
	code    string
	message string
	holders []any
}

// errorOf reads the error object of a refusal and fails on a body that is not the nested shape alone.
func errorOf(t *testing.T, body map[string]any) refused {
	t.Helper()

	object, ok := body["error"].(map[string]any)
	if !ok || len(body) != 1 {
		t.Fatalf("the refusal is %v, want one error object at the root and nothing else", body)
	}
	code, _ := object["code"].(string)
	message, _ := object["message"].(string)
	if code == "" || message == "" {
		t.Fatalf("the error object is %v, want a code and a message", object)
	}
	holders, _ := object["holders"].([]any)

	return refused{code: code, message: message, holders: holders}
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

func TestDaemonIsTheProcessRecordWithTheHandlersVersion(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/daemon")
	if status != http.StatusOK {
		t.Fatalf("GET /v0/daemon answered %d %v", status, body)
	}

	want := map[string]any{
		"version":      "v-test",
		"pid":          float64(4123),
		"started_at":   "2026-09-16T08:00:00Z",
		"socket":       "/var/lib/shard/shard.sock",
		"provider":     "sysbox",
		"capabilities": map[string]any{"pause": false, "resume": false, "fork": false},
		"proxy":        map[string]any{"plain_port": float64(30080), "tls_port": float64(30443)},
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("GET /v0/daemon answered %v, want %v", body, want)
	}
}

func TestDaemonIs500WhenTheProviderCannotBeBuilt(t *testing.T) {
	s := seed(t)

	server := httptest.NewServer(api.NewHandler("v-test", fakeProcess{err: errors.New("find runsc: not on this host")}, s.repo, nil, s.verbs, s.stores, &fakeEgressLog{}, io.Discard))
	t.Cleanup(server.Close)

	status, body := get(t, server, "/v0/daemon")
	if status != http.StatusInternalServerError || errorOf(t, body).message != "find runsc: not on this host" {
		t.Errorf("GET /v0/daemon answered %d %v, want 500 and the provider's error", status, body)
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
	if status != http.StatusBadRequest || !strings.Contains(errorOf(t, body).message, "all=") {
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
	if status != http.StatusInternalServerError || errorOf(t, body).code != "internal" {
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

// A get with ?wait=true blocks on the daemon until the create leaves pending, so it calls WaitState
// before it reads the record; the default get reads at once.
func TestGetWithWaitBlocksOnTheCreate(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes/"+s.running.ID+"?wait=true")
	if status != http.StatusOK || body["id"] != s.running.ID {
		t.Fatalf("GET ?wait answered %d %v, want the record of %s", status, body, s.running.ID)
	}
	if s.verbs.waited != s.running.ID {
		t.Errorf("the get waited on %q, want the ref %s", s.verbs.waited, s.running.ID)
	}

	s.verbs.waited = ""
	get(t, s.server, "/v0/sandboxes/"+s.running.ID)
	if s.verbs.waited != "" {
		t.Errorf("a get with no ?wait blocked on %q, want it to read the record at once", s.verbs.waited)
	}
}

// A wait the daemon fails is the get's answer, and the record read never runs behind it.
func TestGetWithWaitAnswersTheWaitFailure(t *testing.T) {
	s := seed(t)
	s.verbs.err = errors.New("the wait broke")

	status, body := get(t, s.server, "/v0/sandboxes/"+s.running.ID+"?wait=true")
	if status != http.StatusInternalServerError || !strings.Contains(errorOf(t, body).message, "the wait broke") {
		t.Errorf("GET ?wait with a failing wait answered %d %v, want 500 carrying the reason", status, body)
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
	if status != http.StatusNotFound || !strings.Contains(errorOf(t, body).message, "ghost") {
		t.Errorf("GET /v0/sandboxes/ghost answered %d %v, want 404 naming ghost", status, body)
	}
}

func TestGetIs400WhenTheReferenceDoesNotValidate(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes/"+strings.Repeat("a", 65))
	if status != http.StatusBadRequest || !strings.Contains(errorOf(t, body).message, "longer than") {
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
	if status != http.StatusInternalServerError || !strings.Contains(errorOf(t, body).message, "not a sandbox id") {
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
	if status != http.StatusInternalServerError || !strings.Contains(errorOf(t, body).message, "decode") {
		t.Errorf("GET of a corrupt record answered %d %v, want 500", status, body)
	}
}

func TestAnUnknownRouteIsAJSON404(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v1/nothing")
	if refusal := errorOf(t, body); status != http.StatusNotFound || refusal.code != "not_found" || !strings.Contains(refusal.message, "/v1/nothing") {
		t.Errorf("GET /v1/nothing answered %d %v, want a JSON 404 not_found", status, body)
	}
}

func TestTheEgressLogAnswersForAnIDAndForAName(t *testing.T) {
	s := seed(t)

	for _, ref := range []string{s.running.ID, "web"} {
		resp, err := s.server.Client().Get(s.server.URL + "/v0/sandboxes/" + ref + "/egress-log")
		if err != nil {
			t.Fatalf("GET the egress log of %s: %v", ref, err)
		}

		var records []egress.Record
		err = json.NewDecoder(resp.Body).Decode(&records)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode the egress log of %s: %v", ref, err)
		}

		if resp.StatusCode != http.StatusOK || len(records) != 1 || records[0].Host != "web" {
			t.Errorf("GET the egress log of %s answered %d %+v", ref, resp.StatusCode, records)
		}
	}
}

func TestTheEgressLogRefusesAnIDThatNeverExisted(t *testing.T) {
	s := seed(t)

	status, body := get(t, s.server, "/v0/sandboxes/nope/egress-log")
	if status != http.StatusNotFound {
		t.Errorf("GET the egress log of an unknown sandbox answered %d %v", status, body)
	}
}
