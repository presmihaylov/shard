package api_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandbox"
)

// stream is what a plain GET of a follow route answered, the way curl -N asks for it, with the body still open.
type stream struct {
	status      int
	contentType string
	body        *bufio.Reader
}

func follow(t *testing.T, s seeded, path string) stream {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}

	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	return stream{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: bufio.NewReader(resp.Body)}
}

// A follow without the handshake is the bytes as they come, and the body ends with the sandbox.
func TestLogsFollowWithoutTheHandshakeStreamsAsPlainText(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"first\n"}
	s.verbs.stops = make(chan struct{})
	s.verbs.reason = sandbox.LogsStopped

	st := follow(t, s, "/v0/sandboxes/"+s.running.ID+"/logs?follow=true")
	if st.status != http.StatusOK || st.contentType != "text/plain; charset=utf-8" {
		t.Fatalf("the follow answered %d %s, want 200 text/plain", st.status, st.contentType)
	}

	// The first line is out before the sandbox stops, which is what a flush per write buys.
	line, err := st.body.ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("the follow began with %q, %v", line, err)
	}

	close(s.verbs.stops)

	rest, err := io.ReadAll(st.body)
	if err != nil || len(rest) != 0 {
		t.Errorf("after the stop the body carried %q, %v, want nothing and a clean end", rest, err)
	}
}

// A sandbox the daemon does not hold is refused as JSON, follow or not.
func TestLogsFollowWithoutTheHandshakeRefusesAnIDTheDaemonDoesNotHold(t *testing.T) {
	s := seed(t)

	status, body := send(t, s.server, http.MethodGet, "/v0/sandboxes/nosuch/logs?follow=true", "")
	if status != http.StatusNotFound || errorOf(t, body).code != "not_found" {
		t.Errorf("the daemon answered %d %v, want 404 not_found", status, body)
	}
	if s.verbs.followed {
		t.Error("the refusal still reached the orchestrator")
	}
}

// The egress follow without the handshake is one JSON record per line, and a removed sandbox ends it.
func TestEgressLogFollowWithoutTheHandshakeStreamsAsNDJSON(t *testing.T) {
	s := seed(t)

	st := follow(t, s, "/v0/sandboxes/"+s.running.ID+"/egress-log?follow=true")
	if st.status != http.StatusOK || st.contentType != "application/x-ndjson" {
		t.Fatalf("the follow answered %d %s, want 200 application/x-ndjson", st.status, st.contentType)
	}

	body, err := io.ReadAll(st.body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 1 || !strings.HasSuffix(string(body), "\n") {
		t.Fatalf("the body is %q, want one record and its newline", body)
	}

	var record egress.Record
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	if record.Host != s.running.Name {
		t.Errorf("the record is %+v", record)
	}
}

// A stopped sandbox makes no more decisions, so the egress follow ends on the stop, not only on the rm.
func TestEgressLogFollowWithoutTheHandshakeEndsWhenTheSandboxStops(t *testing.T) {
	s := seed(t)
	s.egress.holds = true

	st := follow(t, s, "/v0/sandboxes/"+s.running.ID+"/egress-log?follow=true")
	if _, err := st.body.ReadString('\n'); err != nil {
		t.Fatalf("read the first record: %v", err)
	}

	stop(t, s)

	rest, err := io.ReadAll(st.body)
	if err != nil || len(rest) != 0 {
		t.Errorf("after the stop the body carried %q, %v, want nothing and a clean end", rest, err)
	}
}

func TestEgressLogFollowOverAWebSocketEndsWhenTheSandboxStops(t *testing.T) {
	s := seed(t)
	s.egress.holds = true

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/egress-log?follow=true")
	if _, _, err := conn.Read(t.Context()); err != nil {
		t.Fatalf("read the first record: %v", err)
	}

	stop(t, s)

	_, _, err := conn.Read(t.Context())

	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("the follow went on: %v", err)
	}
	if closeErr.Code != websocket.StatusNormalClosure || !strings.Contains(closeErr.Reason, "stopped") {
		t.Errorf("the follow ended with %d %q, want 1000 saying the sandbox stopped", closeErr.Code, closeErr.Reason)
	}
}

// stop marks the running sandbox stopped in the record, which is all an egress follow watches.
func stop(t *testing.T, s seeded) {
	t.Helper()

	err := s.repo.Update(s.running.ID, func(sb *models.Sandbox) error {
		sb.State = models.StateStopped

		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}
