package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// The snapshot routes and create from a snapshot hold their final shape and answer 501 until SHARD-457a lands.
func TestTheSnapshotRoutesAnswer501NotImplemented(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v0/snapshots", `{"sandbox":"web","name":"base"}`},
		{http.MethodGet, "/v0/snapshots", ""},
		{http.MethodGet, "/v0/snapshots/base", ""},
		{http.MethodDelete, "/v0/snapshots/base", ""},
		{http.MethodPost, "/v0/sandboxes", `{"snapshot":"base","name":"web-2"}`},
	}

	for _, r := range routes {
		s := seed(t)

		status, got := send(t, s.server, r.method, r.path, r.body)
		refusal := errorOf(t, got)
		if status != http.StatusNotImplemented || refusal.code != "not_implemented" || !strings.Contains(refusal.message, "not implemented yet (SHARD-457a)") {
			t.Errorf("%s %s answered %d %v, want 501 not_implemented that names SHARD-457a", r.method, r.path, status, got)
		}
		if s.verbs.created.Snapshot != "" {
			t.Errorf("%s %s reached the orchestrator: %+v", r.method, r.path, s.verbs.created)
		}
	}
}

// The body is part of the contract, so a field the route does not know is still a 400.
func TestASnapshotCreateIs400ForAFieldItDoesNotKnow(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/snapshots", `{"sandbox":"web","label":"base"}`)
	if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
		t.Errorf("POST /v0/snapshots with an unknown field answered %d %v, want 400 invalid_request", status, got)
	}
}
