package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// dial opens the WebSocket of one route, or hands back the refusal the daemon answered instead of the 101.
func dial(t *testing.T, s seeded, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	conn, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(s.server.URL, "http")+path, nil)
	if conn != nil {
		t.Cleanup(func() { conn.CloseNow() })
	}

	return conn, resp, err
}

// open opens the WebSocket of one route and fails the test on anything but the 101.
func open(t *testing.T, s seeded, path string) *websocket.Conn {
	t.Helper()

	conn, resp, err := dial(t, s, path)
	if err != nil {
		t.Fatalf("open %s: %v (%s)", path, err, refusal(t, resp))
	}
	conn.SetReadLimit(api.MaxPayload + 1)

	return conn
}

// refusal is the body a Dial got instead of the 101, for the failure message.
func refusal(t *testing.T, resp *http.Response) string {
	t.Helper()

	if resp == nil {
		return "no response"
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the refusal: %v", err)
	}

	return fmt.Sprintf("%d %s", resp.StatusCode, body)
}

// createExec asks for the exec and hands back its id, which is what the attach names.
func createExec(t *testing.T, s seeded, ref, body string) string {
	t.Helper()

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+ref+"/exec", body)
	if status != http.StatusCreated {
		t.Fatalf("POST exec answered %d %v, want 201", status, got)
	}

	id, _ := got["exec"].(string)
	if id == "" || got["expires_at"] == nil {
		t.Fatalf("the 201 carried %v, want the exec id and when it expires", got)
	}

	return id
}

// session is what one exec said until it ended: the output, and the exit or the failure that ended it.
type session struct {
	out      string
	errOut   string
	messages int
	exit     api.ExitMessage
	failure  api.FailureMessage
	ended    byte
}

// read collects the messages until the exit or the failure, which is what ends every exec.
func read(t *testing.T, conn *websocket.Conn) session {
	t.Helper()

	var got session
	for {
		stream, payload, err := api.Receive(t.Context(), conn)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		got.messages++

		switch stream {
		case api.StreamStdout:
			got.out += string(payload)
		case api.StreamStderr:
			got.errOut += string(payload)
		case api.StreamExit:
			got.ended = stream
			if err := json.Unmarshal(payload, &got.exit); err != nil {
				t.Fatalf("the exit message carried %q: %v", payload, err)
			}

			return got
		case api.StreamFailure:
			got.ended = stream
			if err := json.Unmarshal(payload, &got.failure); err != nil {
				t.Fatalf("the failure message carried %q: %v", payload, err)
			}

			return got
		default:
			t.Fatalf("the daemon sent a message of stream %d", stream)
		}
	}
}

// closed reads past the end of the session and reports the status the daemon closed with.
func closed(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()

	_, _, err := conn.Read(t.Context())
	if err == nil {
		t.Fatal("the daemon sent another message after the end of the session")
	}

	return websocket.CloseStatus(err)
}

func TestExecCreatesThenAttachesAndCarriesTheCommandBothWays(t *testing.T) {
	s := seed(t)
	s.verbs.execID = "1a2b3c4d5e6f7a8b"
	s.verbs.out = "hello\n"
	s.verbs.errOut = "careful\n"
	s.verbs.exit = models.ExitStatus{Code: 7}

	execID := createExec(t, s, s.running.ID, `{"command":["sh","-c","exit 7"],"env":["A=1"],"stdin":true}`)
	if execID != "1a2b3c4d5e6f7a8b" {
		t.Errorf("the 201 named exec %q", execID)
	}
	if want := []string{"sh", "-c", "exit 7"}; strings.Join(s.verbs.exec.Command, " ") != strings.Join(want, " ") {
		t.Errorf("command = %v, want %v", s.verbs.exec.Command, want)
	}

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/exec/"+execID)

	if err := api.Send(t.Context(), conn, api.StreamStdin, []byte("typed\n")); err != nil {
		t.Fatalf("send the input: %v", err)
	}
	if err := api.Send(t.Context(), conn, api.StreamStdinClose, nil); err != nil {
		t.Fatalf("send the end of the input: %v", err)
	}

	got := read(t, conn)
	if got.out != "hello\n" || got.errOut != "careful\n" {
		t.Errorf("stdout = %q, stderr = %q", got.out, got.errOut)
	}
	if got.ended != api.StreamExit || got.exit != (api.ExitMessage{Code: 7}) {
		t.Errorf("the session ended with stream %d and %+v, want the exit 7", got.ended, got.exit)
	}
	if status := closed(t, conn); status != websocket.StatusNormalClosure {
		t.Errorf("the daemon closed with %d, want 1000", status)
	}

	if s.verbs.attachedExec != execID || s.verbs.input != "typed\n" {
		t.Errorf("the command attached to %q and read %q, want %s and typed", s.verbs.attachedExec, s.verbs.input, execID)
	}
}

// One payload is at most 1 MiB, so a longer write arrives as several messages and nothing is lost.
func TestExecSplitsAnOutputOverTheLimit(t *testing.T) {
	s := seed(t)
	s.verbs.execID = "1a2b3c4d5e6f7a8b"
	s.verbs.out = strings.Repeat("x", api.MaxPayload+16)

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b")

	got := read(t, conn)
	if got.out != s.verbs.out {
		t.Errorf("the messages carried %d bytes, want %d", len(got.out), len(s.verbs.out))
	}
	if got.messages != 3 {
		t.Errorf("the session was %d messages, want two of output and the exit", got.messages)
	}
}

// Nothing is on the wire before the command runs, so a refusal is a status and a JSON body like any other.
func TestExecRefusesBeforeThe101(t *testing.T) {
	s := seed(t)
	s.verbs.err = &sandbox.StateError{ID: s.stopped.ID, State: models.StateStopped, Fix: "start it again with shard start " + s.stopped.ID, Code: models.CodeSandboxNotRunning}

	status, body := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.stopped.ID+"/exec", `{"command":["true"]}`)
	if status != http.StatusConflict || body["code"] != "sandbox_not_running" {
		t.Fatalf("the create answered %d %v, want 409 sandbox_not_running", status, body)
	}
	if answer, _ := body["error"].(string); !strings.Contains(answer, "shard start") {
		t.Errorf("the refusal is %q, and it must say what to do", answer)
	}

	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"a second attach":      {&sandbox.AttachedError{ID: "1a2b3c4d5e6f7a8b"}, http.StatusConflict, "in_use"},
		"an exec that expired": {fmt.Errorf("exec 1a2b3c4d5e6f7a8b of sandbox %s: %w", s.running.ID, sandboxstate.ErrNotFound), http.StatusNotFound, "not_found"},
	}
	for name, c := range cases {
		s.verbs.err = c.err

		conn, resp, err := dial(t, s, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b")
		if err == nil || conn != nil {
			t.Fatalf("%s got the 101", name)
		}

		status, body := decodeRefusal(t, resp)
		if status != c.status || body["code"] != c.code {
			t.Errorf("%s answered %d %v, want %d %s", name, status, body, c.status, c.code)
		}
	}
}

// An attach without the handshake is a 400 with a code, not the plain-text refusal the library writes.
func TestAStreamWithoutTheHandshakeIs400(t *testing.T) {
	s := seed(t)

	for _, path := range []string{
		"/v0/sandboxes/" + s.running.ID + "/exec/1a2b3c4d5e6f7a8b",
		"/v0/sandboxes/" + s.running.ID + "/logs?follow=true",
		"/v0/sandboxes/" + s.running.ID + "/egress-log?follow=true",
	} {
		status, body := send(t, s.server, http.MethodGet, path, "")
		if status != http.StatusBadRequest || body["code"] != "websocket_required" {
			t.Errorf("GET %s answered %d %v, want 400 websocket_required", path, status, body)
		}
	}
	if s.verbs.attachedExec != "" || s.verbs.followed {
		t.Error("a request without the handshake still reached the orchestrator")
	}
}

// decodeRefusal is the status and the JSON body a Dial got instead of the 101.
func decodeRefusal(t *testing.T, resp *http.Response) (int, map[string]any) {
	t.Helper()

	if resp == nil {
		t.Fatal("the dial got no response")
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}

	return resp.StatusCode, body
}

// A command that never ran exits with the code a shell answers, and the exit message says why.
func TestExecReportsACommandThatNeverRan(t *testing.T) {
	s := seed(t)
	s.verbs.execErr = &models.CommandNotStartedError{Sandbox: s.running.ID, Reason: "failed to load /bin/nope: no such file or directory", Code: 127}

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b")

	got := read(t, conn)
	if got.ended != api.StreamExit || got.exit.Code != 127 || got.exit.Error != "failed to load /bin/nope: no such file or directory" {
		t.Errorf("the session ended with stream %d and %+v, want the exit 127 with the reason", got.ended, got.exit)
	}
}

// A failure after the 101 has no status to carry it, so it is the last message and then a normal close.
func TestExecReportsAFailureAfterThe101(t *testing.T) {
	s := seed(t)
	s.verbs.execErr = errors.New("runsc: boom")

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b")

	got := read(t, conn)
	if got.ended != api.StreamFailure || got.failure != (api.FailureMessage{Error: "runsc: boom", Code: models.CodeInternal}) {
		t.Errorf("the session ended with stream %d and %+v, want the failure with the code internal", got.ended, got.failure)
	}
	if status := closed(t, conn); status != websocket.StatusNormalClosure {
		t.Errorf("the daemon closed with %d, want 1000", status)
	}
}

// A client that closes first ends the command, as a cancelled context does.
func TestExecEndsTheCommandWhenTheClientCloses(t *testing.T) {
	s := seed(t)
	s.verbs.stops = make(chan struct{})

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b")

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case <-s.verbs.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the command ran on after the client closed")
	}
}

func TestResizeNamesTheExecAndAnswers204(t *testing.T) {
	s := seed(t)

	code, _ := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/exec/1a2b3c4d5e6f7a8b/resize", `{"rows":24,"cols":80}`)
	if code != http.StatusNoContent {
		t.Fatalf("the daemon answered %d, want 204", code)
	}
	if s.verbs.resizedExec != "1a2b3c4d5e6f7a8b" {
		t.Errorf("the daemon resized exec %q", s.verbs.resizedExec)
	}
	if s.verbs.size != (sandbox.TerminalSize{Rows: 24, Cols: 80}) {
		t.Errorf("size = %+v, want 24 by 80", s.verbs.size)
	}
}

func TestLogsAnswerAsPlainText(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"hello\n", "world\n"}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.server.URL+"/v0/sandboxes/"+s.running.ID+"/logs", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}

	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET the output: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the output: %v", err)
	}
	if string(body) != "hello\nworld\n" {
		t.Errorf("the output is %q", body)
	}
	if s.verbs.followed {
		t.Error("a plain GET was followed")
	}
}

// A follow arrives as it happens and ends with the reason, so the client can tell a stop from a rm.
func TestLogsFollowStreamsAndSaysWhyItEnded(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"first\n"}
	s.verbs.stops = make(chan struct{})
	s.verbs.reason = sandbox.LogsStopped

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/logs?follow=true")

	// The first line is on the wire before the sandbox stops, which is what a message per write buys.
	stream, payload, err := api.Receive(t.Context(), conn)
	if err != nil {
		t.Fatalf("read the first line: %v", err)
	}
	if stream != api.StreamStdout || string(payload) != "first\n" {
		t.Errorf("the follow began with stream %d %q", stream, payload)
	}

	close(s.verbs.stops)

	stream, payload, err = api.Receive(t.Context(), conn)
	if err != nil {
		t.Fatalf("read the end: %v", err)
	}
	if stream != api.StreamExit || !bytes.Equal(payload, []byte(`{"reason":"stopped"}`)) {
		t.Errorf("the follow ended with stream %d %q", stream, payload)
	}
	if status := closed(t, conn); status != websocket.StatusNormalClosure {
		t.Errorf("the daemon closed with %d, want 1000", status)
	}
}

// A failure after the 101 has no status to carry it, so it is the last message and then a normal close.
func TestLogsFollowReportsAFailureAfterThe101(t *testing.T) {
	s := seed(t)
	s.verbs.err = errors.New("open the output: boom")

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/logs?follow=true")

	got := read(t, conn)
	if got.ended != api.StreamFailure || got.failure != (api.FailureMessage{Error: "open the output: boom", Code: models.CodeInternal}) {
		t.Errorf("the follow ended with stream %d and %+v, want the failure with the code internal", got.ended, got.failure)
	}
	if status := closed(t, conn); status != websocket.StatusNormalClosure {
		t.Errorf("the daemon closed with %d, want 1000", status)
	}
}

// A sandbox the daemon does not hold is refused before anything is on the wire.
func TestLogsFollowRefusesAnIDTheDaemonDoesNotHold(t *testing.T) {
	s := seed(t)

	conn, resp, err := dial(t, s, "/v0/sandboxes/nosuch/logs?follow=true")
	if err == nil || conn != nil {
		t.Fatal("a sandbox the daemon does not hold got the 101")
	}

	status, body := decodeRefusal(t, resp)
	if status != http.StatusNotFound || body["code"] != "not_found" {
		t.Errorf("the daemon answered %d %v, want 404 not_found", status, body)
	}
	if s.verbs.followed {
		t.Error("the refusal still reached the orchestrator")
	}
}

func TestLogsFollowEndsWhenTheClientCloses(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"first\n"}
	s.verbs.stops = make(chan struct{})

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/logs?follow=true")

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case <-s.verbs.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow ran on after the client closed")
	}
}

// The follow route streams one text message per record, and the close says why the log ended.
func TestEgressLogFollowStreamsTheRecordsAndSaysWhyItEnded(t *testing.T) {
	s := seed(t)

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/egress-log?follow=true")

	kind, payload, err := conn.Read(t.Context())
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	if kind != websocket.MessageText {
		t.Errorf("the record came as a binary message")
	}

	var record egress.Record
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	if record.Host != s.running.Name {
		t.Errorf("the record is %+v", record)
	}

	_, _, err = conn.Read(t.Context())

	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("the follow went on: %v", err)
	}
	if closeErr.Code != websocket.StatusNormalClosure || !strings.Contains(closeErr.Reason, "removed") {
		t.Errorf("the follow ended with %d %q, want 1000 saying the sandbox was removed", closeErr.Code, closeErr.Reason)
	}
}

// A sandbox the daemon does not hold is refused before anything is on the wire.
func TestEgressLogFollowRefusesAnIDTheDaemonDoesNotHold(t *testing.T) {
	s := seed(t)

	conn, resp, err := dial(t, s, "/v0/sandboxes/nosuch/egress-log?follow=true")
	if err == nil || conn != nil {
		t.Fatal("a sandbox the daemon does not hold got the 101")
	}

	status, body := decodeRefusal(t, resp)
	if status != http.StatusNotFound || body["code"] != "not_found" {
		t.Errorf("the daemon answered %d %v, want 404 not_found", status, body)
	}
}
