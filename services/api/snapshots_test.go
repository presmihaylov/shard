package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/presmihaylov/shard/services/sandboxstate"
)

func TestCreateSnapshotAnswers201WithTheSnapshot(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/snapshots", `{"sandbox":"web","name":"base"}`)
	if status != http.StatusCreated || got["id"] != "snap1" || got["name"] != "base" {
		t.Errorf("POST /v0/snapshots answered %d %v, want 201 with the snapshot", status, got)
	}
	if s.verbs.snapshotted.Sandbox != "web" || s.verbs.snapshotted.Name != "base" {
		t.Errorf("the orchestrator got %+v, want the sandbox web and the name base", s.verbs.snapshotted)
	}

	status, got = send(t, s.server, http.MethodPost, "/v0/snapshots", `{"source":"web"}`)
	if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
		t.Errorf("POST /v0/snapshots with an unknown field answered %d %v, want 400", status, got)
	}
}

func TestListSnapshotsPagesByID(t *testing.T) {
	s := seed(t)

	status, got := get(t, s.server, "/v0/snapshots?limit=2")
	snapshots, _ := got["snapshots"].([]any)
	if status != http.StatusOK || len(snapshots) != 2 || got["next"] != "snap2" {
		t.Fatalf("GET /v0/snapshots?limit=2 answered %d %v, want two snapshots and the cursor snap2", status, got)
	}

	_, got = get(t, s.server, "/v0/snapshots?cursor=snap2")
	if snapshots, _ := got["snapshots"].([]any); len(snapshots) != 1 || got["next"] != nil {
		t.Errorf("the page after snap2 is %v, want the last snapshot and no cursor", got)
	}
}

func TestGetAndRemoveSnapshotNameTheReference(t *testing.T) {
	s := seed(t)

	status, got := get(t, s.server, "/v0/snapshots/base")
	if status != http.StatusOK || got["id"] != "snap1" || s.verbs.ref != "base" {
		t.Errorf("GET /v0/snapshots/base answered %d %v for %q, want 200 with the snapshot", status, got, s.verbs.ref)
	}

	s.verbs.ref = ""
	if status, _ := send(t, s.server, http.MethodDelete, "/v0/snapshots/base", ""); status != http.StatusNoContent || s.verbs.ref != "base" {
		t.Errorf("DELETE /v0/snapshots/base answered %d for %q, want 204 for base", status, s.verbs.ref)
	}
}

func TestASnapshotThatIsNotThereIs404(t *testing.T) {
	s := seed(t)
	s.verbs.err = fmt.Errorf("snapshot base: %w", sandboxstate.ErrSnapshotNotFound)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		status, got := send(t, s.server, method, "/v0/snapshots/base", "")
		if status != http.StatusNotFound || errorOf(t, got).code != "not_found" {
			t.Errorf("%s of a snapshot that is not there answered %d %v, want 404", method, status, got)
		}
	}
}

func TestCreateHandsTheSnapshotToTheOrchestrator(t *testing.T) {
	s := seed(t)

	if status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", `{"snapshot":"base"}`); status >= 300 {
		t.Fatalf("POST /v0/sandboxes with a snapshot answered %d %v", status, got)
	}
	if s.verbs.created.Snapshot != "base" || s.verbs.created.Image != "" {
		t.Errorf("the orchestrator got %+v, want the snapshot base and no image", s.verbs.created)
	}
}
