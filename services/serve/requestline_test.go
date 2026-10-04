package serve

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// The front must read the request line as the daemon's net/http does, or a space net/http does not split on shows the front another route than the daemon serves (SHARD-371).
func TestTheFrontReadsTheRequestLineAsTheDaemonDoes(t *testing.T) {
	caps, err := newCapMux()
	if err != nil {
		t.Fatalf("build the scope mux: %v", err)
	}

	for _, line := range []string{
		"GET /v0/sandboxes HTTP/1.1",
		"GET /v0/sandboxes?limit=2 HTTP/1.1",
		"DELETE /v0/images/docker.io/library/alpine:3.20 HTTP/1.1",
		"GET /v0/sandboxes\u00a0/v0/secrets HTTP/1.1",
		"GET\u00a0/v0/sandboxes HTTP/1.1",
		"GET /v0/sandboxes\u00a0HTTP/1.1",
		"GET\t/v0/sandboxes HTTP/1.1",
		"GET /v0/sandboxes\u2003HTTP/1.1",
		"GET /v0/sandboxes HTTP/1.1 trailing",
		"GET /v0/sandboxes SHARD/1.1",
		"G@T /v0/sandboxes HTTP/1.1",
		"GET  /v0/sandboxes HTTP/1.1",
		"CONNECT example.com:443 HTTP/1.1",
		"CONNECT /v0/sandboxes HTTP/1.1",
	} {
		t.Run(line, func(t *testing.T) {
			head := []byte(line + "\r\nHost: shard\r\n\r\n")
			method, target, ok := requestLine(head)

			daemon, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
			if err != nil {
				if ok {
					t.Fatalf("the front read %s %s, and the daemon refuses the line: %v", method, target, err)
				}

				return
			}
			if !ok {
				t.Fatalf("the front refused a line the daemon reads as %s %s", daemon.Method, daemon.URL)
			}

			front, frontKnown := caps.scope(method, target)
			want, wantKnown := caps.scope(daemon.Method, daemon.URL)
			if method != daemon.Method || target.String() != daemon.URL.String() || front != want || frontKnown != wantKnown {
				t.Errorf("the front read %s %s as %q, and the daemon reads %s %s as %q", method, target, front, daemon.Method, daemon.URL, want)
			}
		})
	}
}

// A line the daemon would not parse is the front's own 400, with or without a token, and nothing is dialed.
func TestTheFrontRejectsARequestLineSplitOnAnotherSpace(t *testing.T) {
	up := fakeDaemon(t)
	env := newTokenEnv(t)
	address := front(t, up.root, env.secret)
	token := mint(t, env, "ci")

	for _, auth := range []string{"", "Authorization: Bearer " + token + "\r\n"} {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatalf("dial the front: %v", err)
		}
		defer conn.Close()

		if _, err := io.WriteString(conn, "GET\u00a0/v0/sandboxes HTTP/1.1\r\nHost: box\r\n"+auth+"\r\n"); err != nil {
			t.Fatalf("write the request: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read the answer: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the body: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid_request") {
			t.Errorf("the front answered %d %s with a token %t, want 400 invalid_request", resp.StatusCode, body, auth != "")
		}
	}

	if dialed := up.dialed.Load(); dialed != 0 {
		t.Errorf("the front dialed the socket %d times for a line the daemon would refuse, want none", dialed)
	}
}
