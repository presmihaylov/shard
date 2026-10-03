package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/api"
)

// sendScoped posts a create whose scopes header the front would have stamped; an empty scopes value sends no header.
func sendScoped(t *testing.T, s seeded, scopes, body string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.server.URL+"/v0/sandboxes", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if scopes != "" {
		req.Header.Set(api.ScopesHeader, scopes)
	}

	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v0/sandboxes: %v", err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode the body: %v", err)
	}

	return resp.StatusCode, decoded
}

const (
	bodyWithSecret = `{"image":"alpine:3.20","secrets":["TOKEN"],"resources":{"memory_mib":512,"vcpus":2}}`
	bodyWithPolicy = `{"image":"alpine:3.20","policy":"locked","resources":{"memory_mib":512,"vcpus":2}}`
	bodyWithBoth   = `{"image":"alpine:3.20","secrets":["TOKEN"],"policy":"locked","resources":{"memory_mib":512,"vcpus":2}}`
)

func TestCreateNamingASecretWithoutSecretScopeIs403(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "sandbox:read,sandbox:write,exec", bodyWithSecret)
	if status != http.StatusForbidden {
		t.Fatalf("POST answered %d %v, want 403", status, got)
	}
	assertForbidden(t, got, "secret:*")
	if s.verbs.created.Image != "" {
		t.Fatalf("Create ran on a refused request: %+v", s.verbs.created)
	}
}

func TestCreateNamingAPolicyWithoutPolicyScopeIs403(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "sandbox:read,sandbox:write,exec", bodyWithPolicy)
	if status != http.StatusForbidden {
		t.Fatalf("POST answered %d %v, want 403", status, got)
	}
	assertForbidden(t, got, "policy:*")
	if s.verbs.created.Image != "" {
		t.Fatalf("Create ran on a refused request: %+v", s.verbs.created)
	}
}

func TestCreateWithTheSecretAndPolicyScopesPasses(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "sandbox:write,secret:*,policy:*", bodyWithBoth)
	if status != http.StatusCreated {
		t.Fatalf("POST answered %d %v, want 201", status, got)
	}
}

func TestCreateWithTheStarScopePasses(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "*", bodyWithBoth)
	if status != http.StatusCreated {
		t.Fatalf("POST answered %d %v, want 201", status, got)
	}
}

// No scopes header is the local socket, which keeps every right, so a create naming a secret and a policy passes.
func TestCreateWithNoScopesHeaderKeepsEveryRight(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "", bodyWithBoth)
	if status != http.StatusCreated {
		t.Fatalf("POST answered %d %v, want 201", status, got)
	}
}

func assertForbidden(t *testing.T, got map[string]any, scope string) {
	t.Helper()

	obj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", got)
	}
	if obj["code"] != "forbidden" {
		t.Errorf("code is %v, want forbidden", obj["code"])
	}
	message, _ := obj["message"].(string)
	if !strings.Contains(message, scope) {
		t.Errorf("message %q does not name the missing scope %q", message, scope)
	}
}
