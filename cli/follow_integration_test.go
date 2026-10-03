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
	id := runDetached(t, app, out, "/bin/sh", "-c", "echo marker; exec sleep 600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	contentType, body := newRawClient(t, app).follow("/v0/sandboxes/" + id + "/logs?follow=true")
	if contentType != "text/plain; charset=utf-8" {
		t.Fatalf("the logs follow is %q, want text/plain; charset=utf-8", contentType)
	}

	if line := awaitLine(t, body); line != "marker\n" {
		t.Fatalf("the first line is %q, want marker", line)
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	rest := awaitEnd(t, body)
	if rest != "" {
		t.Errorf("the body carried %q after the stop, want nothing", rest)
	}
}

func TestEgressLogFollowOverPlainHTTPEndsOnTheRemove(t *testing.T) {
	app, out := newCreateApp(t)
	id := runDetached(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	contentType, body := newRawClient(t, app).follow("/v0/sandboxes/" + id + "/egress-log?follow=true")
	if contentType != "application/x-ndjson" {
		t.Fatalf("the egress log follow is %q, want application/x-ndjson", contentType)
	}

	if err := app.Run(t.Context(), []string{"exec", id, "--", "/bin/sh", "-c", "ping -c 1 -W 2 169.254.169.254 >/dev/null 2>&1 || true"}); err != nil {
		t.Fatalf("exec: %v", err)
	}

	awaitDrop(t, body, `"address":"169.254.169.254"`)

	if err := app.Run(t.Context(), []string{"rm", "--force", id}); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	// The guest's own IPv6 drops can still be unread at the rm, so the body ends on whole records, not on nothing.
	for _, line := range strings.SplitAfter(awaitEnd(t, body), "\n") {
		if line != "" && !isHostDrop(line) {
			t.Errorf("the body carried %q after the rm, want whole records of host drops", line)
		}
	}
}

// awaitDrop reads host drops until one carries want, since the guest's own IPv6 traffic drops before the ping does.
func awaitDrop(t *testing.T, body *bufio.Reader, want string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		line := awaitLine(t, body)
		if !isHostDrop(line) {
			t.Fatalf("the follow carried %q, want one JSON record of a host drop", line)
		}
		if strings.Contains(line, want) {
			return
		}
	}

	t.Fatalf("the follow carried no record with %s in 15s", want)
}

func isHostDrop(line string) bool {
	return strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}\n") && strings.Contains(line, `"source":"host"`)
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

// awaitLine reads the first line of a follow, and fails rather than hangs when none comes.
func awaitLine(t *testing.T, body *bufio.Reader) string {
	t.Helper()

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := body.ReadString('\n')
		done <- result{line, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the body ended with %v before its first line, after %q", r.err, r.line)
		}

		return r.line
	case <-time.After(15 * time.Second):
		t.Fatal("the follow carried no line in 15s")
	}

	return ""
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
