package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/image"
)

// sendStreamed asks for the ndjson stream and answers the status, the content type and every line.
func sendStreamed(t *testing.T, server *httptest.Server, path, body string) (int, string, []map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	var lines []map[string]any
	decoder := json.NewDecoder(resp.Body)
	for decoder.More() {
		var line map[string]any
		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("POST %s: decode a line: %v", path, err)
		}
		lines = append(lines, line)
	}

	return resp.StatusCode, resp.Header.Get("Content-Type"), lines
}

func pullEvents() []image.Event {
	return []image.Event{
		{Status: image.StatusPulling, Reference: "docker.io/library/alpine:3.20", Digest: "sha256:beef", Layers: 1, Bytes: 3},
		{Status: image.StatusLayer, Digest: "sha256:cafe", Bytes: 3},
		{Status: image.StatusPulled, Reference: "docker.io/library/alpine:3.20", Digest: "sha256:beef", Path: "/images/alpine"},
	}
}

func statusOf(t *testing.T, line map[string]any) string {
	t.Helper()

	event, ok := line["event"].(map[string]any)
	if !ok || len(line) != 1 {
		t.Fatalf("the line is %v, want one event and nothing else", line)
	}
	status, _ := event["status"].(string)

	return status
}

func TestPullStreamsEachEventThenTheImage(t *testing.T) {
	s := seed(t)
	s.stores.pulled = pullEvents()

	status, contentType, lines := sendStreamed(t, s.server, "/v0/images/pull", `{"ref":"docker.io/library/alpine:3.20"}`)
	if status != http.StatusOK || contentType != "application/x-ndjson" || len(lines) != 4 {
		t.Fatalf("the pull answered %d %s with %v, want 200 ndjson, three events and the image", status, contentType, lines)
	}

	for i, want := range []string{image.StatusPulling, image.StatusLayer, image.StatusPulled} {
		if got := statusOf(t, lines[i]); got != want {
			t.Errorf("line %d is %q, want %q", i, got, want)
		}
	}
	img, ok := lines[3]["image"].(map[string]any)
	if !ok || img["reference"] != "docker.io/library/alpine:3.20" {
		t.Errorf("the last line is %v, want the image", lines[3])
	}
}

// Nothing went on the wire yet, so the refusal keeps the status the plain route answers.
func TestPullStreamRefusesBeforeTheFirstEventWithItsStatus(t *testing.T) {
	s := seed(t)
	s.stores.err = image.ErrBadReference

	status, contentType, lines := sendStreamed(t, s.server, "/v0/images/pull", `{"ref":":::not a ref:::"}`)
	if status != http.StatusBadRequest || !strings.HasPrefix(contentType, "application/json") || len(lines) != 1 {
		t.Fatalf("the pull answered %d %s with %v, want one 400 JSON body", status, contentType, lines)
	}
	if got := errorOf(t, lines[0]).code; got != "invalid_request" {
		t.Errorf("the code is %q, want invalid_request", got)
	}
}

// The 200 is already sent, so a failure after the first event is the last line and carries the code.
func TestPullStreamEndsInAnErrorLineAfterTheFirstEvent(t *testing.T) {
	s := seed(t)
	s.stores.pulled = pullEvents()[:1]
	s.stores.err = errors.New("the registry hung up")

	status, _, lines := sendStreamed(t, s.server, "/v0/images/pull", `{"ref":"docker.io/library/alpine:3.20"}`)
	if status != http.StatusOK || len(lines) != 2 {
		t.Fatalf("the pull answered %d with %v, want 200, the event and the error", status, lines)
	}
	if got := errorOf(t, lines[1]); got.code != "internal" || got.message != "the registry hung up" {
		t.Errorf("the last line is %+v, want internal with the registry's message", got)
	}
}

func TestCreateWaitStreamsThePullThenTheRecord(t *testing.T) {
	s := seed(t)
	s.verbs.createdID = s.running.ID
	s.verbs.pulled = []image.Event{{Status: image.StatusCached, Reference: "docker.io/library/alpine:3.20", Path: "/images/alpine"}}

	status, _, lines := sendStreamed(t, s.server, "/v0/sandboxes?wait=true", `{"image":"alpine:3.20"}`)
	if status != http.StatusCreated || len(lines) != 2 {
		t.Fatalf("the create answered %d with %v, want 201, the event and the record", status, lines)
	}
	if got := statusOf(t, lines[0]); got != image.StatusCached {
		t.Errorf("the first line is %q, want cached", got)
	}
	sb, ok := lines[1]["sandbox"].(map[string]any)
	if !ok || sb["id"] != s.running.ID || s.verbs.waited != s.running.ID {
		t.Errorf("the last line is %v after a wait on %q, want the record of %s", lines[1], s.verbs.waited, s.running.ID)
	}
}
