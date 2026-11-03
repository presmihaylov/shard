package api_test

import (
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestLinkPutAndRemoveNameTheSandboxAndTheTarget(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPut, "/v0/sandboxes/agent1/links/web1", "")
	if status != http.StatusOK || s.verbs.ref != "agent1" || s.verbs.target != "web1" {
		t.Errorf("the put answered %d %v over ref %q and target %q", status, got, s.verbs.ref, s.verbs.target)
	}
	if got["target"] != "web1" || got["address"] != "10.86.0.1" {
		t.Errorf("the put answered the link %v, want web1 at 10.86.0.1", got)
	}

	s.verbs.target = ""
	status, got = send(t, s.server, http.MethodDelete, "/v0/sandboxes/agent1/links/web1", "")
	if status != http.StatusNoContent || s.verbs.target != "web1" {
		t.Errorf("the rm answered %d %v over target %q, want 204 over web1", status, got, s.verbs.target)
	}
}

func TestLinkListAnswersTheLinksOfTheSandbox(t *testing.T) {
	s := seed(t)
	s.verbs.links = []models.Link{{Target: "web1", TargetID: "sandbox2", Address: netip.MustParseAddr("10.86.0.5")}}

	status, got := get(t, s.server, "/v0/sandboxes/agent1/links")
	links, ok := got["links"].([]any)
	if status != http.StatusOK || !ok || len(links) != 1 || s.verbs.ref != "agent1" {
		t.Fatalf("GET /v0/sandboxes/agent1/links answered %d %v over ref %q, want the one link", status, got, s.verbs.ref)
	}
	link, ok := links[0].(map[string]any)
	if !ok || link["target"] != "web1" || link["address"] != "10.86.0.5" {
		t.Errorf("the link reads %v, want web1 at 10.86.0.5", links[0])
	}
}

func TestLinkListAnswersNoLinksAsAnArray(t *testing.T) {
	s := seed(t)

	status, got := get(t, s.server, "/v0/sandboxes/agent1/links")
	links, ok := got["links"].([]any)
	if status != http.StatusOK || !ok || len(links) != 0 {
		t.Errorf("GET /v0/sandboxes/agent1/links answered %d %v, want an empty links array", status, got)
	}
}

func TestLinkRefusalsAnswerTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		err                error
		status             int
		code, message      string
	}{
		{"a target the sandbox holds no link to", http.MethodDelete, "/v0/sandboxes/agent1/links/web1", &sandbox.LinkNotFoundError{Sandbox: "agent1", Target: "web1"}, http.StatusNotFound, "not_found", "sandbox agent1 holds no link to web1: shard link list agent1 lists its links"},
		{"a target that does not exist", http.MethodPut, "/v0/sandboxes/agent1/links/ghost", &sandbox.RequestError{Err: errors.New("sandbox ghost does not exist")}, http.StatusBadRequest, "invalid_request", "sandbox ghost does not exist"},
		{"a target that failed", http.MethodPut, "/v0/sandboxes/agent1/links/web1", &sandbox.StateError{Sandbox: "web1", State: models.StateFailed, Code: models.CodeSandboxFailed}, http.StatusConflict, "sandbox_failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := seed(t)
			s.verbs.err = tc.err

			status, got := send(t, s.server, tc.method, tc.path, "")
			refusal := errorOf(t, got)
			if status != tc.status || refusal.code != tc.code {
				t.Errorf("%s %s answered %d %v, want %d %s", tc.method, tc.path, status, got, tc.status, tc.code)
			}
			if tc.message != "" && refusal.message != tc.message {
				t.Errorf("%s %s answered %q, want %q", tc.method, tc.path, refusal.message, tc.message)
			}
		})
	}
}

const bodyWithLink = `{"image":"alpine:3.20","links":["web1"],"resources":{"memory_mib":512,"vcpus":2}}`

func TestCreateLinkingASandboxWithoutSSHIs403(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "sandbox:write,exec", bodyWithLink)
	if status != http.StatusForbidden {
		t.Fatalf("POST answered %d %v, want 403", status, got)
	}
	assertForbidden(t, got, "ssh")
	if s.verbs.created.Image != "" {
		t.Fatalf("Create ran on a refused request: %+v", s.verbs.created)
	}

	status, got = sendScoped(t, s, "sandbox:write,ssh", bodyWithLink)
	if status != http.StatusCreated || !slices.Equal(s.verbs.created.Links, []string{"web1"}) {
		t.Errorf("POST with ssh answered %d %v and created %+v, want 201 with the link", status, got, s.verbs.created)
	}
}

func TestCreateLinkingOnTheLocalSocketKeepsEveryRight(t *testing.T) {
	s := seed(t)

	status, got := sendScoped(t, s, "", bodyWithLink)
	if status != http.StatusCreated || !slices.Equal(s.verbs.created.Links, []string{"web1"}) {
		t.Errorf("POST with no scopes header answered %d %v and created %+v, want 201 with the link", status, got, s.verbs.created)
	}
}
