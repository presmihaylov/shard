package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/secret"
)

// fakeStores answers every store verb with err, and records what the route handed it.
type fakeStores struct {
	err error

	name  string
	ref   string
	force bool
	rules []sandbox.RuleText
	// value is what a secret PUT carried, which nothing else may ever hold.
	value        string
	destinations []string
	mock         string

	policies []models.Policy
	secrets  []secret.Secret
	images   []image.Image
	warnings []string
	removed  []string
	// listErr is what the secret list reports beside the secrets that read.
	listErr error
}

func (f *fakeStores) SetPolicy(_ context.Context, name string, req sandbox.PolicyRequest) (models.Policy, error) {
	f.name, f.rules = name, req.Rules
	if f.err != nil {
		return models.Policy{}, f.err
	}

	return models.Policy{Name: name}, nil
}

func (f *fakeStores) Policy(name string) (models.Policy, error) {
	f.name = name
	if f.err != nil {
		return models.Policy{}, f.err
	}

	return models.Policy{Name: name}, nil
}

func (f *fakeStores) Policies() ([]models.Policy, error) { return f.policies, f.err }

func (f *fakeStores) RemovePolicy(name string) error {
	f.name = name

	return f.err
}

func (f *fakeStores) SetSecret(name string, req sandbox.SecretRequest) (secret.Secret, error) {
	f.name, f.value, f.destinations, f.mock = name, req.Value, req.Destinations, req.MockValue
	if f.err != nil {
		return secret.Secret{}, f.err
	}

	return secret.Secret{Name: name, Destinations: req.Destinations, MockValue: req.MockValue}, nil
}

func (f *fakeStores) Secrets() ([]secret.Secret, error) { return f.secrets, f.listErr }

func (f *fakeStores) RemoveSecret(name string, force bool) error {
	f.name, f.force = name, force

	return f.err
}

func (f *fakeStores) PullImage(_ context.Context, ref string) (image.Image, error) {
	f.ref = ref
	if f.err != nil {
		return image.Image{}, f.err
	}

	return image.Image{Reference: ref, Digest: "sha256:beef"}, nil
}

func (f *fakeStores) Images() ([]image.Image, error) { return f.images, f.err }

func (f *fakeStores) RemoveImage(_ context.Context, ref string, force bool) ([]string, error) {
	f.ref, f.force = ref, force

	return f.warnings, f.err
}

func (f *fakeStores) PruneImages(context.Context) ([]string, []string, error) {
	return f.removed, f.warnings, f.err
}

func TestPolicyRoutesAnswerTheStore(t *testing.T) {
	s := seed(t)
	s.stores.policies = []models.Policy{{Name: "web"}, {Name: "db"}}

	status, body := get(t, s.server, "/v0/policies")
	rows, ok := body["policies"].([]any)
	if status != http.StatusOK || !ok || len(rows) != 2 {
		t.Fatalf("GET /v0/policies answered %d %v, want both policies", status, body)
	}

	status, body = get(t, s.server, "/v0/policies/web")
	if status != http.StatusOK || body["name"] != "web" {
		t.Errorf("GET /v0/policies/web answered %d %v", status, body)
	}
}

func TestPutPolicyHandsTheRulesOverInOrder(t *testing.T) {
	s := seed(t)

	body := `{"rules":[{"action":"allow","rule":"api.example.com"},{"action":"deny","rule":"any"}]}`
	status, answer := send(t, s.server, http.MethodPut, "/v0/policies/web", body)
	if status != http.StatusOK || answer["name"] != "web" {
		t.Fatalf("PUT /v0/policies/web answered %d %v", status, answer)
	}

	want := []sandbox.RuleText{
		{Action: models.ActionAllow, Rule: "api.example.com"},
		{Action: models.ActionDeny, Rule: "any"},
	}
	if s.stores.name != "web" || len(s.stores.rules) != 2 || s.stores.rules[0] != want[0] || s.stores.rules[1] != want[1] {
		t.Errorf("the store got %s %v, want %v", s.stores.name, s.stores.rules, want)
	}
}

func TestARuleTheDaemonRefusesIsABadRequest(t *testing.T) {
	s := seed(t)
	s.stores.err = &sandbox.RequestError{Err: errors.New("not a rule")}

	status, body := send(t, s.server, http.MethodPut, "/v0/policies/web", `{"rules":[{"action":"allow","rule":"???"}]}`)
	if status != http.StatusBadRequest || !strings.Contains(body["error"].(string), "not a rule") {
		t.Errorf("PUT of a bad rule answered %d %v, want 400", status, body)
	}
}

func TestAMissingPolicyIsNotFound(t *testing.T) {
	s := seed(t)
	s.stores.err = egress.ErrNotFound

	status, body := get(t, s.server, "/v0/policies/gone")
	if status != http.StatusNotFound {
		t.Errorf("GET of a missing policy answered %d %v, want 404", status, body)
	}
}

func TestRemovingAHeldPolicyIsAConflictThatNamesTheHolders(t *testing.T) {
	s := seed(t)
	s.stores.err = &sandbox.HeldError{Subject: "policy web", Verb: "held by", Users: []string{"quiet-heron-3f0a"}, Fix: "remove the sandbox first"}

	status, body := send(t, s.server, http.MethodDelete, "/v0/policies/web", "")
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "quiet-heron-3f0a") {
		t.Errorf("DELETE of a held policy answered %d %v, want 409 naming the holder", status, body)
	}
}

func TestRemovingAPolicyAnswers204(t *testing.T) {
	s := seed(t)

	status, _ := send(t, s.server, http.MethodDelete, "/v0/policies/web", "")
	if status != http.StatusNoContent || s.stores.name != "web" {
		t.Errorf("DELETE /v0/policies/web answered %d for %q", status, s.stores.name)
	}
}

// A value never leaves the host, so the list route answers with names and destinations alone.
func TestSecretListCarriesNoValue(t *testing.T) {
	s := seed(t)
	s.stores.secrets = []secret.Secret{{
		Name:         "openai",
		Destinations: []string{"api.example.com"},
		MockValue:    "sk-mock",
		UpdatedAt:    time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
	}}
	s.stores.listErr = errors.New("secret broken.json does not decode")

	status, body := get(t, s.server, "/v0/secrets")
	if status != http.StatusOK {
		t.Fatalf("GET /v0/secrets answered %d %v", status, body)
	}

	rows, ok := body["secrets"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("the body holds %v, want one secret", body["secrets"])
	}
	row := rows[0].(map[string]any)
	if _, ok := row["value"]; ok {
		t.Error("the list answered with a value")
	}
	if row["name"] != "openai" || row["mock_value"] != "sk-mock" {
		t.Errorf("the row is %v", row)
	}

	warnings, ok := body["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Errorf("the warnings are %v, want the file that does not decode", body["warnings"])
	}
}

// The value crosses the socket on this route and on no other, and the answer carries it back nowhere.
func TestPutSecretCarriesTheValueOnceAndAnswersWithout(t *testing.T) {
	s := seed(t)

	body := `{"value":"sk-live-synthetic","destinations":["api.example.com"],"mock_value":"sk-mock"}`
	status, answer := send(t, s.server, http.MethodPut, "/v0/secrets/openai", body)
	if status != http.StatusOK {
		t.Fatalf("PUT /v0/secrets/openai answered %d %v", status, answer)
	}
	if s.stores.value != "sk-live-synthetic" || s.stores.mock != "sk-mock" {
		t.Errorf("the store got value %q and placeholder %q", s.stores.value, s.stores.mock)
	}
	if _, ok := answer["value"]; ok {
		t.Errorf("the answer carries the value: %v", answer)
	}
	if answer["name"] != "openai" {
		t.Errorf("the answer is %v", answer)
	}
}

func TestRemovingAGrantedSecretIsAConflictUnlessForced(t *testing.T) {
	s := seed(t)
	s.stores.err = &sandbox.HeldError{Subject: "secret openai", Verb: "granted to", Users: []string{"quiet-heron-3f0a"}, Fix: "remove the sandbox first, or pass --force"}

	status, body := send(t, s.server, http.MethodDelete, "/v0/secrets/openai", "")
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "quiet-heron-3f0a") {
		t.Fatalf("DELETE of a granted secret answered %d %v, want 409", status, body)
	}

	s.stores.err = nil
	status, _ = send(t, s.server, http.MethodDelete, "/v0/secrets/openai?force=true", "")
	if status != http.StatusNoContent || !s.stores.force {
		t.Errorf("DELETE with force answered %d, forced %v", status, s.stores.force)
	}
}

func TestAMissingSecretIsNotFound(t *testing.T) {
	s := seed(t)
	s.stores.err = secret.ErrNotFound

	status, body := send(t, s.server, http.MethodDelete, "/v0/secrets/gone", "")
	if status != http.StatusNotFound {
		t.Errorf("DELETE of a missing secret answered %d %v, want 404", status, body)
	}
}

func TestImageListAndPull(t *testing.T) {
	s := seed(t)
	s.stores.images = []image.Image{{Reference: "docker.io/library/alpine:3.20", Digest: "sha256:beef", Size: 2048}}

	status, body := get(t, s.server, "/v0/images")
	rows, ok := body["images"].([]any)
	if status != http.StatusOK || !ok || len(rows) != 1 {
		t.Fatalf("GET /v0/images answered %d %v", status, body)
	}

	status, body = send(t, s.server, http.MethodPost, "/v0/images/pull", `{"ref":"docker.io/library/alpine:3.20"}`)
	if status != http.StatusOK || body["reference"] != "docker.io/library/alpine:3.20" {
		t.Errorf("POST /v0/images/pull answered %d %v", status, body)
	}
}

// A reference carries slashes, so the route must hand the store the whole of it and not one segment.
func TestRemovingAnImageKeepsTheWholeReference(t *testing.T) {
	s := seed(t)
	s.stores.warnings = []string{"a blob was not reclaimed"}

	status, body := send(t, s.server, http.MethodDelete, "/v0/images/docker.io/library/alpine:3.20?force=true", "")
	if status != http.StatusOK {
		t.Fatalf("DELETE of an image answered %d %v", status, body)
	}
	if s.stores.ref != "docker.io/library/alpine:3.20" || !s.stores.force {
		t.Errorf("the store got %q, forced %v", s.stores.ref, s.stores.force)
	}

	warnings, ok := body["warnings"].([]any)
	if !ok || len(warnings) != 1 {
		t.Errorf("the warnings are %v", body["warnings"])
	}
}

func TestRemovingAReferencedImageIsAConflict(t *testing.T) {
	s := seed(t)
	s.stores.err = &sandbox.HeldError{Subject: "image alpine:3.20", Verb: "referenced by", Users: []string{"quiet-heron-3f0a"}, Fix: "remove the sandbox first, or pass --force"}

	status, body := send(t, s.server, http.MethodDelete, "/v0/images/alpine:3.20", "")
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "quiet-heron-3f0a") {
		t.Errorf("DELETE of a referenced image answered %d %v, want 409", status, body)
	}
}

func TestAMissingImageIsNotFound(t *testing.T) {
	s := seed(t)
	s.stores.err = image.ErrNotFound

	status, body := send(t, s.server, http.MethodDelete, "/v0/images/alpine:3.20", "")
	if status != http.StatusNotFound {
		t.Errorf("DELETE of a missing image answered %d %v, want 404", status, body)
	}
}

func TestPruneAnswersWhatItRemovedAndWhatItLeft(t *testing.T) {
	s := seed(t)
	s.stores.removed = []string{"docker.io/library/alpine:3.20"}
	s.stores.warnings = []string{"a blob was not reclaimed"}

	status, body := send(t, s.server, http.MethodPost, "/v0/images/prune", "")
	if status != http.StatusOK {
		t.Fatalf("POST /v0/images/prune answered %d %v", status, body)
	}

	removed, ok := body["removed"].([]any)
	if !ok || len(removed) != 1 || removed[0] != "docker.io/library/alpine:3.20" {
		t.Errorf("the removed images are %v", body["removed"])
	}
	if warnings, ok := body["warnings"].([]any); !ok || len(warnings) != 1 {
		t.Errorf("the warnings are %v", body["warnings"])
	}
}
