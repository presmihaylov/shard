package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/sandbox"
)

// A body far past the cap is refused with 413 after the cap, so the daemon never holds the rest of it.
func TestABodyPastTheCapIs413AndAllocatesFlat(t *testing.T) {
	cases := []struct {
		name, prefix string
		pad          byte
	}{
		{"one value", `{"x":"`, 'a'},
		{"a valid value and padding", `{}`, ' '},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := seed(t)

			const size = 64 << 20
			conn, err := net.Dial("tcp", s.server.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial the server: %v", err)
			}
			t.Cleanup(func() { conn.Close() })

			var before runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			head := fmt.Sprintf("PUT /v0/policies/p HTTP/1.1\r\nHost: shard\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", size+len(c.prefix), c.prefix)
			if _, err := io.WriteString(conn, head); err != nil {
				t.Fatalf("write the request head: %v", err)
			}

			// One chunk written again and again keeps the client's own allocation out of the measure.
			written := make(chan struct{})
			go func() {
				defer close(written)

				chunk := bytes.Repeat([]byte{c.pad}, 64<<10)
				for sent := 0; sent < size; sent += len(chunk) {
					if _, err := conn.Write(chunk); err != nil {
						return
					}
				}
			}()

			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read the answer: %v", err)
			}
			defer resp.Body.Close()

			var got map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode the answer: %v", err)
			}

			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			conn.Close()
			<-written

			if resp.StatusCode != http.StatusRequestEntityTooLarge || errorOf(t, got).code != "body_too_large" {
				t.Errorf("a %d byte body answered %d %v, want 413 body_too_large", size, resp.StatusCode, got)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
				t.Errorf("a %d byte body allocated %d bytes, want under 16 MiB whatever the size", size, grew)
			}
			if s.stores.name != "" {
				t.Errorf("the refused body still reached the store as %q", s.stores.name)
			}
		})
	}
}

// A second value after the first is refused, so a body is one value and no verb runs on its head alone.
func TestASecondValueIsRefused(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPut, "/v0/policies/p", `{} {}`)
	if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
		t.Errorf("two values answered %d %v, want 400 invalid_request", status, got)
	}
	if s.stores.name != "" {
		t.Errorf("the refused body still reached the store as %q", s.stores.name)
	}
}

// slow serves the seeded handler with a ReadTimeout far shorter than the verbs it times.
func slow(t *testing.T, s seeded) seeded {
	t.Helper()

	server := httptest.NewUnstartedServer(s.handler)
	server.Config.ReadTimeout = 50 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	s.server = server

	return s
}

// The ReadTimeout bounds a slow body only, so a create that pulls for longer still runs to its answer.
func TestACreateOutlivesTheReadTimeout(t *testing.T) {
	s := slow(t, seed(t))
	s.verbs.hold = 200 * time.Millisecond

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", `{"image":"alpine"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST answered %d %v, want 201", status, got)
	}
	if s.verbs.heldErr != nil {
		t.Errorf("the create's context ended mid-create with %v, want it live past the ReadTimeout", s.verbs.heldErr)
	}
}

// A follow has no body, so the ReadTimeout never applies to it and it ends with the sandbox.
func TestAFollowOutlivesTheReadTimeout(t *testing.T) {
	s := slow(t, seed(t))
	s.verbs.lines = []string{"first\n"}
	s.verbs.stops = make(chan struct{})
	s.verbs.reason = sandbox.LogsStopped

	st := follow(t, s, "/v0/sandboxes/"+s.running.ID+"/logs?follow=true")
	line, err := st.body.ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("the follow began with %q, %v", line, err)
	}

	time.Sleep(200 * time.Millisecond)
	select {
	case <-s.verbs.ended:
		t.Fatal("the follow ended at the ReadTimeout, want it open until the sandbox stops")
	default:
	}

	close(s.verbs.stops)

	rest, err := io.ReadAll(st.body)
	if err != nil || len(rest) != 0 {
		t.Errorf("after the stop the body carried %q, %v, want nothing and a clean end", rest, err)
	}
}
