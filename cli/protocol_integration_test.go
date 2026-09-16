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

// expiryMargin is how long past expires_at an attach may still find the exec before the test calls it kept.
const expiryMargin = 20 * time.Second

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

// createExec posts the request and answers the ticket of the 201.
func (c rawClient) createExec(id string, req sandbox.ExecRequest) sandbox.ExecTicket {
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

	var ticket sandbox.ExecTicket
	if err := json.NewDecoder(resp.Body).Decode(&ticket); err != nil {
		c.t.Fatalf("decode the ticket: %v", err)
	}
	if ticket.ID == "" {
		c.t.Fatal("the ticket names no exec")
	}

	return ticket
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

// plainGet asks the attach without the handshake, which is a JSON refusal and no exec.
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

// The wire is the contract: stream bytes both ways, the exit as JSON, and a close of 1000 after it.
func TestExecProtocolCarriesTheStreamsAndTheExitJSON(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	before := time.Now()
	ticket := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/sh", "-c", "cat; echo err >&2; exit 3"}, Stdin: true})

	if until := ticket.ExpiresAt.Sub(before); until < sandbox.DefaultExecExpiry-5*time.Second || until > sandbox.DefaultExecExpiry+5*time.Second {
		t.Errorf("the exec expires in %s, want about %s", until, sandbox.DefaultExecExpiry)
	}

	// The handshake is the whole difference between a refusal and a session, and the refusal keeps the exec.
	if status, code := c.plainGet("/v0/sandboxes/" + id + "/exec/" + ticket.ID); status != http.StatusBadRequest || code != models.CodeWebSocketRequired {
		t.Errorf("an attach without the handshake answered %d %s, want 400 %s", status, code, models.CodeWebSocketRequired)
	}

	conn, status, _ := c.dial(id, ticket.ID)
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

	ticket := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/cat"}, Stdin: true})

	conn, status, _ := c.dial(id, ticket.ID)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("the attach answered %d, want 101", status)
	}

	if _, status, code := c.dial(id, ticket.ID); status != http.StatusConflict || code != models.CodeInUse {
		t.Errorf("the second attach answered %d %s, want 409 %s", status, code, models.CodeInUse)
	}

	c.send(conn, api.StreamStdinClose, "")
	if s := c.read(conn); s.exit != (api.ExitMessage{}) {
		t.Errorf("the exit was %+v, want code 0", s.exit)
	}

	if _, status, code := c.dial(id, ticket.ID); status != http.StatusNotFound || code != models.CodeNotFound {
		t.Errorf("an attach after the end answered %d %s, want 404 %s", status, code, models.CodeNotFound)
	}
}

// An exec nobody attaches is dropped at expires_at, and the attach that comes late is a 404.
func TestExecProtocolForgetsAnExecNobodyAttached(t *testing.T) {
	app, id := runningSandbox(t)
	c := newRawClient(t, app)

	ticket := c.createExec(id, sandbox.ExecRequest{Command: []string{"/bin/true"}})

	time.Sleep(time.Until(ticket.ExpiresAt))

	deadline := ticket.ExpiresAt.Add(expiryMargin)
	for {
		_, status, code := c.dial(id, ticket.ID)
		if status == http.StatusNotFound && code == models.CodeNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the attach answered %d %s at %s past expires_at, want 404", status, code, expiryMargin)
		}

		time.Sleep(time.Second)
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
