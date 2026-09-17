package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// getAndRm are the only two sandbox-scoped routes a failed sandbox still answers; every other one is 409.
var getAndRm = map[api.Route]bool{
	{Method: http.MethodGet, Pattern: "/v0/sandboxes/{id}"}:    true,
	{Method: http.MethodDelete, Pattern: "/v0/sandboxes/{id}"}: true,
}

// The walk reads api.Routes, so a route added to the daemon is covered here without an edit to this test.
func TestEveryVerbButGetAndRmIs409OnAFailedSandbox(t *testing.T) {
	s := seed(t)
	failed := create(t, s.repo, "broken", models.StateFailed)
	s.verbs.err = &sandbox.StateError{ID: failed.ID, State: models.StateFailed, Fix: "remove it", Code: models.CodeSandboxFailed}

	subst := strings.NewReplacer("{id}", failed.ID, "{exec}", "e1", "{name}", "n1")
	walked := 0
	for _, route := range api.Routes() {
		if !strings.Contains(route.Pattern, "{id}") || getAndRm[route] {
			continue
		}
		walked++

		path := subst.Replace(route.Pattern)
		status, body := send(t, s.server, route.Method, path, "")
		if status != http.StatusConflict {
			t.Errorf("%s %s on a failed sandbox answered %d, want 409", route.Method, route.Pattern, status)

			continue
		}
		if got := errorOf(t, body).code; got != string(models.CodeSandboxFailed) {
			t.Errorf("%s %s answered code %q, want %q", route.Method, route.Pattern, got, models.CodeSandboxFailed)
		}
	}

	// A filter that matched nothing would pass in silence, so the walk proves it covered the sandbox verbs.
	if walked < 15 {
		t.Fatalf("the walk covered %d sandbox routes, want the full set", walked)
	}
}

// A failed sandbox is still readable, so an operator sees why it failed before removing it.
func TestGetIsAllowedOnAFailedSandbox(t *testing.T) {
	s := seed(t)
	failed := create(t, s.repo, "broken", models.StateFailed)

	status, got := get(t, s.server, "/v0/sandboxes/"+failed.ID)
	if status != http.StatusOK || got["state"] != "failed" {
		t.Fatalf("GET on a failed sandbox answered %d %v, want 200 with the failed record", status, got)
	}
}

// The egress log reads around the Service, so its handler carries the guard the lifecycle verbs get for free.
func TestEgressLogOnAFailedSandboxIs409(t *testing.T) {
	s := seed(t)
	failed := create(t, s.repo, "broken", models.StateFailed)

	for _, path := range []string{"/v0/sandboxes/" + failed.ID + "/egress-log", "/v0/sandboxes/" + failed.ID + "/egress-log?follow=true"} {
		status, body := get(t, s.server, path)
		if status != http.StatusConflict || errorOf(t, body).code != string(models.CodeSandboxFailed) {
			t.Errorf("GET %s answered %d %v, want 409 sandbox_failed", path, status, body)
		}
	}
}

// The follow log streams around the Service too, so both follow paths refuse a failed sandbox before the 200 or the 101.
func TestLogsFollowOnAFailedSandboxIs409(t *testing.T) {
	s := seed(t)
	failed := create(t, s.repo, "broken", models.StateFailed)
	path := "/v0/sandboxes/" + failed.ID + "/logs?follow=true"

	// The plain follow (no WebSocket handshake) is refused before the 200.
	status, body := get(t, s.server, path)
	if status != http.StatusConflict || errorOf(t, body).code != string(models.CodeSandboxFailed) {
		t.Errorf("plain follow answered %d %v, want 409 sandbox_failed", status, body)
	}

	// The WebSocket follow is refused before the 101, not with a failure frame after it.
	conn, resp, err := dial(t, s, path)
	if err == nil || conn != nil {
		t.Fatal("a failed sandbox got the 101")
	}
	status, body = decodeRefusal(t, resp)
	if status != http.StatusConflict || errorOf(t, body).code != string(models.CodeSandboxFailed) {
		t.Errorf("websocket follow answered %d %v, want 409 sandbox_failed", status, body)
	}

	if s.verbs.followed {
		t.Error("the refusal still reached the orchestrator")
	}
}
