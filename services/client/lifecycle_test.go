package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// seen is what one request carried, for a test to read back once the call returned.
type seen struct {
	method, uri, contentType string
	body                     []byte
}

// echo answers status and body, and records every request it saw.
func echo(status int, body string, saw *seen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		*saw = seen{method: r.Method, uri: r.URL.RequestURI(), contentType: r.Header.Get("Content-Type"), body: raw}
		answer(status, body)(w, r)
	}
}

func TestCreateSandboxPostsTheRequestAndDecodesTheRecord(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusCreated, `{"id":"sandbox1","state":"running"}`, &saw))

	req := sandbox.CreateRequest{Image: "alpine:3.20", Name: "web", Secrets: []string{"TOKEN"}, Resources: models.Resources{MemoryMiB: 512}}

	sb, err := c.CreateSandbox(t.Context(), req)
	if err != nil || sb.ID != "sandbox1" || sb.State != models.StateRunning {
		t.Fatalf("CreateSandbox = %+v, %v; want sandbox1 running", sb, err)
	}

	if saw.method != http.MethodPost || saw.uri != "/v0/sandboxes" || saw.contentType != "application/json" {
		t.Errorf("the request was %s %s as %q, want POST /v0/sandboxes as JSON", saw.method, saw.uri, saw.contentType)
	}

	var got sandbox.CreateRequest
	if err := json.Unmarshal(saw.body, &got); err != nil {
		t.Fatalf("the body %s is not a request: %v", saw.body, err)
	}
	if got.Image != "alpine:3.20" || got.Name != "web" || got.Secrets[0] != "TOKEN" || got.Resources.MemoryMiB != 512 {
		t.Errorf("the daemon got %+v, want %+v", got, req)
	}
}

// A create pulls the image inside the daemon, and no per-request deadline can say how long that takes.
func TestCreateSandboxOutlivesTheClientTimeout(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		answer(http.StatusCreated, `{"id":"sandbox1"}`)(w, r)
	})
	c.Timeout = 20 * time.Millisecond

	if _, err := c.CreateSandbox(t.Context(), sandbox.CreateRequest{Image: "alpine"}); err != nil {
		t.Errorf("CreateSandbox = %v, want the record after the pull however long it took", err)
	}
}

func TestStartSandboxPostsToTheReference(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusOK, `{"id":"sandbox1","state":"running"}`, &saw))

	sb, err := c.StartSandbox(t.Context(), "web")
	if err != nil || sb.ID != "sandbox1" {
		t.Fatalf("StartSandbox = %+v, %v; want sandbox1", sb, err)
	}
	if saw.method != http.MethodPost || saw.uri != "/v0/sandboxes/web/start" || len(saw.body) != 0 {
		t.Errorf("the request was %s %s with %q, want POST /v0/sandboxes/web/start with no body", saw.method, saw.uri, saw.body)
	}
}

func TestStopSandboxSendsTheGraceInSeconds(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusOK, `{"id":"sandbox1","state":"stopped"}`, &saw))

	sb, err := c.StopSandbox(t.Context(), "web", 2500*time.Millisecond)
	if err != nil || sb.State != models.StateStopped {
		t.Fatalf("StopSandbox = %+v, %v; want stopped", sb, err)
	}
	if saw.uri != "/v0/sandboxes/web/stop" || string(saw.body) != `{"grace":2.5}` {
		t.Errorf("the request was %s with %q, want the stop route with the grace in seconds", saw.uri, saw.body)
	}
}

// The stop waits the grace out before it answers, so the deadline is the grace on top of the timeout.
func TestStopSandboxWaitsTheGraceOnTopOfTheTimeout(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		answer(http.StatusOK, `{"id":"sandbox1"}`)(w, r)
	})
	c.Timeout = 20 * time.Millisecond

	if _, err := c.StopSandbox(t.Context(), "sandbox1", time.Second); err != nil {
		t.Errorf("StopSandbox = %v, want the record once the grace ran out", err)
	}
}

func TestRemoveSandboxDeletesWithForceAndGraceOnlyWhenForced(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusNoContent, "", &saw))

	if err := c.RemoveSandbox(t.Context(), "web", false, time.Second); err != nil {
		t.Fatalf("RemoveSandbox = %v", err)
	}
	if saw.method != http.MethodDelete || saw.uri != "/v0/sandboxes/web" {
		t.Errorf("the request was %s %s, want DELETE /v0/sandboxes/web", saw.method, saw.uri)
	}

	if err := c.RemoveSandbox(t.Context(), "web", true, 3*time.Second); err != nil {
		t.Fatalf("RemoveSandbox --force = %v", err)
	}
	if saw.uri != "/v0/sandboxes/web?force=true&grace=3" {
		t.Errorf("the forced request was %s, want the force and the grace in the query", saw.uri)
	}
}

func TestTheLifecycleVerbsTurnA404IntoNotFound(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found"}`))

	calls := map[string]func() error{
		"start":  func() error { _, err := c.StartSandbox(t.Context(), "ghost"); return err },
		"stop":   func() error { _, err := c.StopSandbox(t.Context(), "ghost", time.Second); return err },
		"remove": func() error { return c.RemoveSandbox(t.Context(), "ghost", false, time.Second) },
	}

	for verb, call := range calls {
		err := call()

		var missing *client.NotFoundError
		if !errors.As(err, &missing) || missing.Ref != "ghost" {
			t.Errorf("%s = %v, want a NotFoundError for ghost", verb, err)
		}
	}
}

// A 409 is the daemon's refusal in its own words, which the CLI prints as it came.
func TestAConflictCarriesTheDaemonsMessage(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusConflict, `{"error":"sandbox sandbox1 is running: stop it first with shard stop sandbox1, or pass --force"}`))

	err := c.RemoveSandbox(t.Context(), "sandbox1", false, time.Second)
	if err == nil || err.Error() != "sandbox sandbox1 is running: stop it first with shard stop sandbox1, or pass --force" {
		t.Errorf("RemoveSandbox = %v, want the daemon's message as it came", err)
	}
}
