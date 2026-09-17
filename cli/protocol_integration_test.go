//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// rawClient speaks the wire protocol to the daemon of the package, with no services/client in between.
type rawClient struct {
	t    *testing.T
	http *http.Client
}

func newRawClient(t *testing.T, app App) rawClient {
	t.Helper()

	socket := filepath.Join(app.Root, api.SocketFile)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}

	return rawClient{t: t, http: &http.Client{Transport: transport}}
}

// createExec posts the request and answers the exec record of the 201, which is already running.
func (c rawClient) createExec(id string, req sandbox.ExecRequest) models.Exec {
	c.t.Helper()

	body, err := json.Marshal(req)
	if err != nil {
		c.t.Fatalf("encode the exec request: %v", err)
	}

	resp, err := c.http.Post("http://shard/v0/sandboxes/"+id+"/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("POST the exec: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("POST the exec answered %d, want 201", resp.StatusCode)
	}

	var exec models.Exec
	if err := json.NewDecoder(resp.Body).Decode(&exec); err != nil {
		c.t.Fatalf("decode the exec: %v", err)
	}
	if exec.ID == "" {
		c.t.Fatal("the record names no exec")
	}
	if exec.State != models.ExecRunning {
		c.t.Fatalf("the exec is %q, want running", exec.State)
	}

	return exec
}

// dial opens the attach, or answers the status and code of the refusal that came before the 101.
func (c rawClient) dial(id, execID string) (*websocket.Conn, int, models.Code) {
	c.t.Helper()

	conn, resp, err := websocket.Dial(c.t.Context(), "ws://shard/v0/sandboxes/"+id+"/exec/"+execID, &websocket.DialOptions{HTTPClient: c.http})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		conn.SetReadLimit(api.MaxPayload + 1)
		c.t.Cleanup(func() { conn.CloseNow() })

		return conn, resp.StatusCode, ""
	}
	if resp == nil {
		c.t.Fatalf("dial the attach: %v", err)
	}

	var refusal struct {
		Error struct {
			Code models.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
		c.t.Fatalf("decode the refusal of the attach: %v", err)
	}

	return nil, resp.StatusCode, refusal.Error.Code
}

// getRecord asks for the exec record without a handshake, which is the record as it stands now.
func (c rawClient) getRecord(id, execID string) (int, models.Exec) {
	c.t.Helper()

	resp, err := c.http.Get("http://shard/v0/sandboxes/" + id + "/exec/" + execID)
	if err != nil {
		c.t.Fatalf("GET the record: %v", err)
	}
	defer resp.Body.Close()

	var exec models.Exec
	if err := json.NewDecoder(resp.Body).Decode(&exec); err != nil {
		c.t.Fatalf("decode the record: %v", err)
	}

	return resp.StatusCode, exec
}

// plainGet does a GET with no handshake and answers the status and the refusal code of a non-2xx body.
func (c rawClient) plainGet(path string) (int, models.Code) {
	c.t.Helper()

	resp, err := c.http.Get("http://shard" + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	var refusal struct {
		Error struct {
			Code models.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
		c.t.Fatalf("decode the answer of GET %s: %v", path, err)
	}

	return resp.StatusCode, refusal.Error.Code
}

// send writes one message of stream with payload.
func (c rawClient) send(conn *websocket.Conn, stream byte, payload string) {
	c.t.Helper()

	if err := api.Send(c.t.Context(), conn, stream, []byte(payload)); err != nil {
		c.t.Fatalf("send stream %d: %v", stream, err)
	}
}

// session is what the daemon sent on one exec, up to and including the message that ended it.
type session struct {
	out, errOut bytes.Buffer
	exit        api.ExitMessage
}

// read collects the output until the exit, then proves the daemon closed with 1000.
func (c rawClient) read(conn *websocket.Conn) session {
	c.t.Helper()

	var s session
	for {
		stream, payload, err := api.Receive(c.t.Context(), conn)
		if err != nil {
			c.t.Fatalf("receive: %v", err)
		}

		switch stream {
		case api.StreamStdout:
			s.out.Write(payload)
		case api.StreamStderr:
			s.errOut.Write(payload)
		case api.StreamExit:
			if err := json.Unmarshal(payload, &s.exit); err != nil {
				c.t.Fatalf("decode the exit %q: %v", payload, err)
			}
			if status := c.closed(conn); status != websocket.StatusNormalClosure {
				c.t.Errorf("the daemon closed with %d after the exit, want 1000", status)
			}

			return s
		default:
			c.t.Fatalf("the daemon sent stream %d with %q", stream, payload)
		}
	}
}

// closed reads past the end and answers the close status the daemon sent.
func (c rawClient) closed(conn *websocket.Conn) websocket.StatusCode {
	c.t.Helper()

	_, _, err := conn.Read(c.t.Context())
	if err == nil {
		c.t.Fatal("the daemon sent another message after the exit")
	}

	return websocket.CloseStatus(err)
}

// reattachBudget bounds the wait for the daemon to free the slot a dropped client held.
const reattachBudget = 5 * time.Second

// readUntil collects the output until stdout holds want, so a test can drop a client mid-command.
func (c rawClient) readUntil(conn *websocket.Conn, want string) {
	c.t.Helper()

	var out bytes.Buffer
	for !strings.Contains(out.String(), want) {
		stream, payload, err := api.Receive(c.t.Context(), conn)
		if err != nil {
			c.t.Fatalf("receive before %q: %v", want, err)
		}
		if stream == api.StreamStdout {
			out.Write(payload)
		}
	}
}

// reattach dials until the daemon frees the slot the dropped client held, because it lets go a moment later.
func (c rawClient) reattach(id, execID string) *websocket.Conn {
	c.t.Helper()

	deadline := time.After(reattachBudget)
	for {
		conn, status, code := c.dial(id, execID)
		if status == http.StatusSwitchingProtocols {
			return conn
		}
		if status != http.StatusConflict || code != models.CodeInUse {
			c.t.Fatalf("the re-attach answered %d %s, want 101", status, code)
		}

		select {
		case <-deadline:
			c.t.Fatalf("the daemon held the attach slot past %s after the drop", reattachBudget)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// kill posts one signal to the exec and proves the daemon answered 204.
func (c rawClient) kill(id, execID, signal string) {
	c.t.Helper()

	body := bytes.NewReader([]byte(`{"signal":"` + signal + `"}`))
	resp, err := c.http.Post("http://shard/v0/sandboxes/"+id+"/exec/"+execID+"/kill", "application/json", body)
	if err != nil {
		c.t.Fatalf("POST the kill: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		c.t.Fatalf("the kill answered %d, want 204", resp.StatusCode)
	}
}

// The wire is the contract: stream bytes both ways, the exit as JSON, and a close of 1000 after it.
func TestExecProtocolCarriesTheStreamsAndTheExitJSON(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	exec := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/sh", "-c", "cat; echo err >&2; exit 3"}, Stdin: true})

	// The command runs from the create, so a plain GET is the record, and the handshake is what attaches.
	if status, got := c.getRecord(id, exec.ID); status != http.StatusOK || got.State != models.ExecRunning {
		t.Errorf("a GET without the handshake answered %d and state %q, want 200 running", status, got.State)
	}

	conn, status, _ := c.dial(id, exec.ID)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("the attach answered %d, want 101", status)
	}

	c.send(conn, api.StreamStdin, "hello from the wire\n")
	c.send(conn, api.StreamStdinClose, "")

	s := c.read(conn)
	if s.out.String() != "hello from the wire\n" {
		t.Errorf("stdout carried %q, want what stdin sent", s.out.String())
	}
	if s.errOut.String() != "err\n" {
		t.Errorf("stderr carried %q, want err", s.errOut.String())
	}
	if s.exit != (api.ExitMessage{Code: 3}) {
		t.Errorf("the exit was %+v, want code 3", s.exit)
	}
}

// One attach per exec: the second is refused before the 101, and the first keeps running.
func TestExecProtocolRefusesASecondAttach(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	exec := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/cat"}, Stdin: true})

	conn, status, _ := c.dial(id, exec.ID)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("the attach answered %d, want 101", status)
	}

	if _, status, code := c.dial(id, exec.ID); status != http.StatusConflict || code != models.CodeInUse {
		t.Errorf("the second attach answered %d %s, want 409 %s", status, code, models.CodeInUse)
	}

	c.send(conn, api.StreamStdinClose, "")
	if s := c.read(conn); s.exit != (api.ExitMessage{}) {
		t.Errorf("the exit was %+v, want code 0", s.exit)
	}

	// The record outlives the command, so an attach after the end replays it and answers the exit again.
	after, status, _ := c.dial(id, exec.ID)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("an attach after the end answered %d, want 101", status)
	}
	if s := c.read(after); s.exit != (api.ExitMessage{}) {
		t.Errorf("the replay after the end ended with %+v, want code 0", s.exit)
	}
}

// An exec the sandbox stop takes with it is gone, so an attach after the stop is a 404.
func TestExecProtocolForgetsAnExecWhenTheSandboxStops(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	exec := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/cat"}, Stdin: true})

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop the sandbox: %v", err)
	}

	if _, status, code := c.dial(id, exec.ID); status != http.StatusNotFound || code != models.CodeNotFound {
		t.Errorf("an attach after the stop answered %d %s, want 404 %s", status, code, models.CodeNotFound)
	}
}

// A client that drops mid-command does not end it: the exec runs on, and a re-attach replays the bytes
// from before the drop, then carries the rest to the exit.
func TestExecProtocolReplaysAfterAMidCommandDrop(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	exec := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/sh", "-c", "echo mark; cat"}, Stdin: true})

	first, status, _ := c.dial(id, exec.ID)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("the attach answered %d, want 101", status)
	}
	c.readUntil(first, "mark\n")
	first.CloseNow()

	second := c.reattach(id, exec.ID)
	c.send(second, api.StreamStdinClose, "")

	s := c.read(second)
	if !strings.Contains(s.out.String(), "mark\n") {
		t.Errorf("the re-attach replayed %q, want the mark from before the drop", s.out.String())
	}
	if s.exit != (api.ExitMessage{}) {
		t.Errorf("the exit was %+v, want code 0", s.exit)
	}
}

// A kill ends a running command with the signal's code: TERM makes a sleep exit 143, which is 128 and 15.
func TestExecProtocolKillEndsACommandWithTheSignalCode(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	exec := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/sh", "-c", "sleep 30"}})

	conn, status, _ := c.dial(id, exec.ID)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("the attach answered %d, want 101", status)
	}

	c.kill(id, exec.ID, "TERM")

	if s := c.read(conn); s.exit != (api.ExitMessage{Code: 143}) {
		t.Errorf("the exit was %+v, want code 143 from the TERM", s.exit)
	}
}

// A follow of the output is the same wire: log bytes on stream 1, and the reason it ended on stream 3.
func TestLogsProtocolFollowsThenSaysWhyItEnded(t *testing.T) {
	app, out := newCreateApp(t)
	c := newRawClient(t, app)

	id := create(t, app, out, "/bin/sh", "-c", "echo up; sleep 600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	conn, _, err := websocket.Dial(t.Context(), "ws://shard/v0/sandboxes/"+id+"/logs?follow=true", &websocket.DialOptions{HTTPClient: c.http}) //nolint:bodyclose // a 101 has no body to close
	if err != nil {
		t.Fatalf("dial the follow: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	var logged strings.Builder
	for !strings.Contains(logged.String(), "up\n") {
		stream, payload, err := api.Receive(t.Context(), conn)
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		if stream != api.StreamStdout {
			t.Fatalf("the follow sent stream %d with %q before the entrypoint wrote", stream, payload)
		}

		logged.Write(payload)
	}

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	end, err := readEnd(t.Context(), conn)
	if err != nil {
		t.Fatalf("read to the end of the follow: %v", err)
	}
	if end.Reason != sandbox.LogsStopped {
		t.Errorf("the follow ended with %q, want %s", end.Reason, sandbox.LogsStopped)
	}
	if status := c.closed(conn); status != websocket.StatusNormalClosure {
		t.Errorf("the daemon closed with %d after the end, want 1000", status)
	}
}

// readEnd skips what the entrypoint still wrote and answers the end message.
func readEnd(ctx context.Context, conn *websocket.Conn) (api.EndMessage, error) {
	for {
		stream, payload, err := api.Receive(ctx, conn)
		if err != nil {
			return api.EndMessage{}, err
		}
		if stream != api.StreamExit {
			continue
		}

		var end api.EndMessage
		if err := json.Unmarshal(payload, &end); err != nil {
			return api.EndMessage{}, fmt.Errorf("decode the end %q: %w", payload, err)
		}

		return end, nil
	}
}
