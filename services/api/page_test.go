package api_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/secret"
)

// next reads the cursor of the page after, which every list carries, null at the end.
func next(t *testing.T, body map[string]any) string {
	t.Helper()

	cursor, ok := body["next"]
	if !ok {
		t.Fatalf("the list carries no next: %v", body)
	}
	if cursor == nil {
		return ""
	}

	return cursor.(string)
}

func TestListPagesThroughTheSandboxesOnTheLimitAndTheCursor(t *testing.T) {
	s := seed(t)
	create(t, s.repo, "", models.StateRunning)
	create(t, s.repo, "", models.StateRunning)

	status, body := get(t, s.server, "/v0/sandboxes")
	all := ids(t, body)
	if status != http.StatusOK || len(all) != 3 || next(t, body) != "" {
		t.Fatalf("GET /v0/sandboxes answered %d %v, want the three running sandboxes and a null next", status, body)
	}

	status, body = get(t, s.server, "/v0/sandboxes?limit=2")
	if status != http.StatusOK || !reflect.DeepEqual(ids(t, body), all[:2]) || next(t, body) != all[1] {
		t.Fatalf("GET /v0/sandboxes?limit=2 answered %d %v, want the first two and the second id as next", status, body)
	}

	status, body = get(t, s.server, "/v0/sandboxes?limit=2&cursor="+all[1])
	if status != http.StatusOK || !reflect.DeepEqual(ids(t, body), all[2:]) || next(t, body) != "" {
		t.Errorf("the second page answered %d %v, want the last sandbox and a null next", status, body)
	}

	status, body = get(t, s.server, "/v0/sandboxes?cursor="+all[0])
	if status != http.StatusOK || !reflect.DeepEqual(ids(t, body), all[1:]) || next(t, body) != "" {
		t.Errorf("a cursor with no limit answered %d %v, want everything after it and a null next", status, body)
	}
}

func TestListWalksPastASandboxRemovedBetweenTwoPages(t *testing.T) {
	s := seed(t)
	create(t, s.repo, "", models.StateRunning)
	create(t, s.repo, "", models.StateRunning)

	_, body := get(t, s.server, "/v0/sandboxes")
	all := ids(t, body)

	_, body = get(t, s.server, "/v0/sandboxes?limit=2")
	cursor := next(t, body)
	if err := s.repo.Delete(cursor); err != nil {
		t.Fatalf("Delete %s: %v", cursor, err)
	}

	status, body := get(t, s.server, "/v0/sandboxes?limit=2&cursor="+cursor)
	if status != http.StatusOK || !reflect.DeepEqual(ids(t, body), []string{all[2]}) || next(t, body) != "" {
		t.Errorf("the page after a removed cursor answered %d %v, want the sandbox after it and a null next", status, body)
	}
}

func TestListRefusesALimitThatIsNoCountAndACursorThatIsNoID(t *testing.T) {
	s := seed(t)

	for _, query := range []string{"limit=0", "limit=-1", "limit=ten"} {
		status, body := get(t, s.server, "/v0/sandboxes?"+query)
		if status != http.StatusBadRequest {
			t.Errorf("GET /v0/sandboxes?%s answered %d %v, want 400", query, status, body)

			continue
		}
		refusal := errorOf(t, body)
		if refusal.code != "invalid_request" || !strings.Contains(refusal.message, strings.TrimPrefix(query, "limit=")) {
			t.Errorf("GET /v0/sandboxes?%s refused with %v, want invalid_request naming the value", query, refusal)
		}
	}

	status, body := get(t, s.server, "/v0/sandboxes?cursor=not/an-id")
	if status != http.StatusBadRequest || errorOf(t, body).code != "invalid_request" {
		t.Errorf("GET /v0/sandboxes?cursor=not/an-id answered %d %v, want 400 invalid_request", status, body)
	}
}

func TestEveryStoreListPagesTheSameWay(t *testing.T) {
	s := seed(t)
	s.stores.policies = []models.Policy{{Name: "db"}, {Name: "web"}}
	s.stores.secrets = []secret.Secret{{Name: "OPENAI_KEY"}, {Name: "STRIPE_KEY"}}
	s.stores.images = []image.Image{{Reference: "index.docker.io/library/alpine:3.20"}, {Reference: "index.docker.io/library/debian:13"}}

	for _, route := range []struct{ path, plural, key, first, between, second, malformed string }{
		{"/v0/policies", "policies", "name", "db", "m", "web", "Not-Lower"},
		{"/v0/secrets", "secrets", "name", "OPENAI_KEY", "P", "STRIPE_KEY", "lower"},
		{"/v0/images", "images", "reference", "index.docker.io/library/alpine:3.20", "index.docker.io/library/busybox:1", "index.docker.io/library/debian:13", "Alpine:3.20"},
	} {
		status, body := get(t, s.server, route.path+"?limit=1")
		if status != http.StatusOK || keys(t, body, route.plural, route.key)[0] != route.first || next(t, body) != route.first {
			t.Errorf("GET %s?limit=1 answered %d %v, want %s alone and its key as next", route.path, status, body, route.first)
		}

		status, body = get(t, s.server, route.path+"?limit=1&cursor="+route.first)
		if status != http.StatusOK || keys(t, body, route.plural, route.key)[0] != route.second || next(t, body) != "" {
			t.Errorf("the second page of %s answered %d %v, want %s alone and a null next", route.path, status, body, route.second)
		}

		status, body = get(t, s.server, route.path+"?cursor="+route.between)
		if status != http.StatusOK || keys(t, body, route.plural, route.key)[0] != route.second || next(t, body) != "" {
			t.Errorf("GET %s?cursor=%s answered %d %v, want what sorts after the cursor", route.path, route.between, status, body)
		}

		status, body = get(t, s.server, route.path+"?cursor="+route.malformed)
		if status != http.StatusBadRequest || errorOf(t, body).code != "invalid_request" {
			t.Errorf("GET %s?cursor=%s answered %d %v, want 400 invalid_request", route.path, route.malformed, status, body)
		}
	}
}

// keys reads the field each row of a store list is paged by.
func keys(t *testing.T, body map[string]any, plural, key string) []string {
	t.Helper()

	rows, ok := body[plural].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("the body holds no %s rows: %v", plural, body)
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.(map[string]any)[key].(string))
	}

	return out
}
