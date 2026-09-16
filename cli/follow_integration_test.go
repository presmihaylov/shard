//go:build integration

package cli

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// SHARD-164: a follow without the WebSocket handshake is a chunked body, so curl -N reads it and the body ends on its own.
func TestLogsFollowOverPlainHTTPEndsOnTheStop(t *testing.T) {
	app, out := newCreateApp(t)
	id := create(t, app, out, "/bin/sh", "-c", "echo marker; exec sleep 600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	contentType, body := newRawClient(t, app).follow("/v0/sandboxes/" + id + "/logs?follow=true")
	if contentType != "text/plain; charset=utf-8" {
		t.Fatalf("the logs follow is %q, want text/plain; charset=utf-8", contentType)
	}

	line, err := body.ReadString('\n')
	if err != nil || line != "marker\n" {
		t.Fatalf("the first line is %q, %v, want marker", line, err)
	}

	if err := app.Run(t.Context(), []string{"stop", "--time", "1s", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	rest := awaitEnd(t, body)
	if rest != "" {
		t.Errorf("the body carried %q after the stop, want nothing", rest)
	}
}

func TestEgressLogFollowOverPlainHTTPEndsOnTheRemove(t *testing.T) {
	app, out := newCreateApp(t)
	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	contentType, body := newRawClient(t, app).follow("/v0/sandboxes/" + id + "/egress-log?follow=true")
	if contentType != "application/x-ndjson" {
		t.Fatalf("the egress log follow is %q, want application/x-ndjson", contentType)
	}

	if err := app.Run(t.Context(), []string{"exec", id, "--", "/bin/sh", "-c", "ping -c 1 -W 2 169.254.169.254 >/dev/null 2>&1 || true"}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	line, err := body.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "{") || !strings.Contains(line, `"source":"host"`) {
		t.Fatalf("the first line is %q, %v, want one JSON record of the host drop", line, err)
	}

	if err := app.Run(t.Context(), []string{"rm", "--force", id}); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	if rest := awaitEnd(t, body); rest != "" {
		t.Errorf("the body carried %q after the rm, want nothing", rest)
	}
}

// follow opens a plain GET of a follow, which must answer 200 before the first byte, and leaves the body open.
func (c rawClient) follow(path string) (string, *bufio.Reader) {
	c.t.Helper()

	resp, err := c.http.Get("http://shard" + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	c.t.Cleanup(func() { resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s answered %d, want 200", path, resp.StatusCode)
	}

	return resp.Header.Get("Content-Type"), bufio.NewReader(resp.Body)
}

// awaitEnd reads the body to its end, which the daemon must reach on its own, and answers what was left.
func awaitEnd(t *testing.T, body io.Reader) string {
	t.Helper()

	type result struct {
		rest []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		rest, err := io.ReadAll(body)
		done <- result{rest, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the body ended with %v, want a clean end", r.err)
		}

		return string(r.rest)
	case <-time.After(15 * time.Second):
		t.Fatal("the body outlived the sandbox")
	}

	return ""
}
