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

func TestListRefusesALimitThatIsNoCountAndACursorThatNamesNothing(t *testing.T) {
	s := seed(t)

	for _, query := range []string{"limit=0", "limit=-1", "limit=ten", "cursor=nothing-here"} {
		status, body := get(t, s.server, "/v0/sandboxes?"+query)
		if status != http.StatusBadRequest {
			t.Errorf("GET /v0/sandboxes?%s answered %d %v, want 400", query, status, body)

			continue
		}
		refusal := errorOf(t, body)
		if refusal.code != "invalid_request" || !strings.Contains(refusal.message, strings.SplitN(query, "=", 2)[1]) {
			t.Errorf("GET /v0/sandboxes?%s refused with %v, want invalid_request naming the value", query, refusal)
		}
	}
}

func TestEveryStoreListPagesTheSameWay(t *testing.T) {
	s := seed(t)
	s.stores.policies = []models.Policy{{Name: "db"}, {Name: "web"}}
	s.stores.secrets = []secret.Secret{{Name: "openai"}, {Name: "stripe"}}
	s.stores.images = []image.Image{{Reference: "docker.io/library/alpine:3.20"}, {Reference: "docker.io/library/debian:13"}}

	for _, route := range []struct{ path, plural, key, first, second string }{
		{"/v0/policies", "policies", "name", "db", "web"},
		{"/v0/secrets", "secrets", "name", "openai", "stripe"},
		{"/v0/images", "images", "reference", "docker.io/library/alpine:3.20", "docker.io/library/debian:13"},
	} {
		status, body := get(t, s.server, route.path+"?limit=1")
		if status != http.StatusOK || keys(t, body, route.plural, route.key)[0] != route.first || next(t, body) != route.first {
			t.Errorf("GET %s?limit=1 answered %d %v, want %s alone and its key as next", route.path, status, body, route.first)
		}

		status, body = get(t, s.server, route.path+"?limit=1&cursor="+route.first)
		if status != http.StatusOK || keys(t, body, route.plural, route.key)[0] != route.second || next(t, body) != "" {
			t.Errorf("the second page of %s answered %d %v, want %s alone and a null next", route.path, status, body, route.second)
		}

		status, body = get(t, s.server, route.path+"?cursor=nothing-here")
		if status != http.StatusBadRequest || errorOf(t, body).code != "invalid_request" {
			t.Errorf("GET %s?cursor=nothing-here answered %d %v, want 400 invalid_request", route.path, status, body)
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
