package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// warnBudget is how long a warning nobody wants has to arrive before the test says it never came.
const warnBudget = 2 * time.Second

// execDaemon answers one exec the way the daemon does: the create with a ticket, the attach over a WebSocket.
type execDaemon struct {
	t      *testing.T
	execID string

	out    string
	errOut string
	// exit or failure is the message that ends the session; hangUp ends it with neither, as a daemon that died does.
	exit    *api.ExitMessage
	failure *api.FailureMessage
	hangUp  bool
	// skipInput answers and exits without reading, the way a command that exited at once does.
	skipInput bool
	// waits reads the input until the client goes away, the way a command that never exits does.
	waits bool

	// req is what the client asked for, attached the path it opened, and input what it typed at the command.
	req      sandbox.ExecRequest
	attached string
	input    string
}

func (d *execDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&d.req); err != nil {
			d.t.Errorf("decode the exec request: %v", err)

			return
		}

		answer(http.StatusCreated, `{"exec":"`+d.execID+`","expires_at":"2026-09-16T08:01:00Z"}`)(w, r)

		return
	}

	d.attached = r.URL.Path

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		d.t.Errorf("accept the attach: %v", err)

		return
	}
	defer conn.CloseNow()

	if !d.skipInput {
		d.input = d.readInput(conn)
	}
	if d.hangUp || d.waits {
		return
	}

	d.send(conn, api.StreamStdout, []byte(d.out))
	d.send(conn, api.StreamStderr, []byte(d.errOut))

	if d.failure != nil {
		d.send(conn, api.StreamFailure, mustJSON(d.t, d.failure))
	}
	if d.exit != nil {
		d.send(conn, api.StreamExit, mustJSON(d.t, d.exit))
	}

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		d.t.Errorf("close the attach: %v", err)
	}
}

func (d *execDaemon) send(conn *websocket.Conn, stream byte, payload []byte) {
	if len(payload) == 0 {
		return
	}

	if err := api.Send(context.Background(), conn, stream, payload); err != nil {
		d.t.Errorf("send a message of stream %d: %v", stream, err)
	}
}

// readInput collects what the client typed until it says the keyboard has ended, or it goes away.
func (d *execDaemon) readInput(conn *websocket.Conn) string {
	var typed strings.Builder

	for {
		stream, payload, err := api.Receive(context.Background(), conn)
		if err != nil {
			return typed.String()
		}

		switch stream {
		case api.StreamStdin:
			typed.Write(payload)
		case api.StreamStdinClose:
			return typed.String()
		default:
			d.t.Errorf("the client sent a message of stream %d", stream)

			return typed.String()
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestExecCreatesThenAttachesAndReportsTheExitStatus(t *testing.T) {
	daemon := &execDaemon{t: t, execID: "1a2b3c4d5e6f7a8b", out: "hello\n", errOut: "careful\n", exit: &api.ExitMessage{Code: 7}}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	var out, errOut bytes.Buffer
	var named string

	req := sandbox.ExecRequest{Command: []string{"sh", "-c", "exit 7"}, Env: []string{"A=1"}, WorkDir: "/srv"}
	streams := client.ExecStreams{
		Stdin:   strings.NewReader("typed\n"),
		Stdout:  &out,
		Stderr:  &errOut,
		Started: func(execID string) { named = execID },
	}

	status, err := c.Exec(t.Context(), "sandbox1", req, streams)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if status.Code != 7 {
		t.Errorf("exit code = %d, want 7", status.Code)
	}
	if out.String() != "hello\n" || errOut.String() != "careful\n" {
		t.Errorf("stdout = %q, stderr = %q", out.String(), errOut.String())
	}
	if named != "1a2b3c4d5e6f7a8b" || daemon.attached != "/v0/sandboxes/sandbox1/exec/1a2b3c4d5e6f7a8b" {
		t.Errorf("the exec was named %q and attached at %q", named, daemon.attached)
	}
	if daemon.input != "typed\n" {
		t.Errorf("the daemon read %q, want typed", daemon.input)
	}
	if strings.Join(daemon.req.Command, " ") != "sh -c exit 7" || daemon.req.WorkDir != "/srv" || !daemon.req.Stdin {
		t.Errorf("the daemon was asked for %+v", daemon.req)
	}
}

// A front checks the token per connection, and the attach is a connection of its own.
func TestExecCarriesTheTokenOfAFrontOnEveryConnection(t *testing.T) {
	daemon := &execDaemon{t: t, execID: "1a2b3c4d5e6f7a8b", exit: &api.ExitMessage{}, skipInput: true}

	seen := make(chan string, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		daemon.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	c, err := client.NewRemote(server.URL, "front-token-value", ca)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	if _, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{}); err != nil {
		t.Fatalf("Exec through a front: %v", err)
	}

	for _, step := range []string{"the create", "the attach"} {
		if got := <-seen; got != "Bearer front-token-value" {
			t.Errorf("%s carried %q, want the bearer token", step, got)
		}
	}
}

// A command that never ran exits with the code a shell answers and a reason, and rebuilds as the typed error.
func TestExecReportsACommandThatNeverRan(t *testing.T) {
	daemon := &execDaemon{t: t, exit: &api.ExitMessage{Code: 127, Error: "failed to load /bin/nope: no such file or directory"}}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"/bin/nope"}}, client.ExecStreams{})

	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) {
		t.Fatalf("Exec returned %v, want a command that never ran", err)
	}
	if notStarted.Code != 127 || notStarted.Reason != daemon.exit.Error || notStarted.Sandbox != "sandbox1" {
		t.Errorf("Exec returned %+v, want 127 with the reason in sandbox1", notStarted)
	}
	if daemon.req.Stdin {
		t.Error("the daemon was asked for stdin, and the client had none")
	}
}

// A failure after the 101 arrives as the last message, and rebuilds as the error a refusal would have been.
func TestExecReportsAFailureAfterThe101(t *testing.T) {
	daemon := &execDaemon{t: t, failure: &api.FailureMessage{Error: "runsc: boom", Code: models.CodeInternal}}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})

	var failure *client.APIError
	if !errors.As(err, &failure) || failure.Code != models.CodeInternal || failure.Message != "runsc: boom" {
		t.Fatalf("Exec returned %v, want the daemon's failure with its code", err)
	}
}

// Nothing is on the wire before the command runs, so a refusal is still a status and a JSON body.
func TestExecReportsARefusalOfTheCreate(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusConflict, `{"error":"sandbox sandbox1 is stopped: start it again with shard start sandbox1","code":"sandbox_not_running"}`))

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})

	var refusal *client.APIError
	if !errors.As(err, &refusal) || refusal.Code != models.CodeSandboxNotRunning || !strings.Contains(err.Error(), "shard start sandbox1") {
		t.Fatalf("Exec returned %v, want the daemon's refusal", err)
	}
}

// An attach the daemon refuses is a status and a JSON body instead of the 101, and it reads like any refusal.
func TestExecReportsARefusalOfTheAttach(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			answer(http.StatusCreated, `{"exec":"1a2b3c4d5e6f7a8b","expires_at":"2026-09-16T08:01:00Z"}`)(w, r)

			return
		}

		answer(http.StatusConflict, `{"error":"exec 1a2b3c4d5e6f7a8b is already attached: create another","code":"in_use"}`)(w, r)
	})

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})

	var refusal *client.APIError
	if !errors.As(err, &refusal) || refusal.Code != models.CodeInUse || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("Exec returned %v, want the daemon's refusal", err)
	}
}

func TestExecReportsAnIDTheDaemonDoesNotHold(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found","code":"not_found"}`))

	_, err := c.Exec(t.Context(), "ghost", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})

	var missing *client.NotFoundError
	if !errors.As(err, &missing) || missing.Ref != "ghost" {
		t.Fatalf("Exec returned %v, want no sandbox ghost", err)
	}
}

// Every exec ends with an exit or a failure, so a session that ends with neither is a failure and not a zero.
func TestExecReportsAnExecThatEndedWithNoStatus(t *testing.T) {
	daemon := &execDaemon{t: t, hangUp: true}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})
	if err == nil || !strings.Contains(err.Error(), "without an exit status") {
		t.Fatalf("Exec returned %v, want the missing exit status named", err)
	}
}

// An interrupt ends the session, which is how a client kills the command it was running.
func TestExecEndsWhenTheContextDoes(t *testing.T) {
	daemon := &execDaemon{t: t, waits: true}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	ctx, cancel := context.WithCancel(t.Context())
	streams := client.ExecStreams{Started: func(string) { cancel() }}

	_, err := c.Exec(ctx, "sandbox1", sandbox.ExecRequest{Command: []string{"sleep", "600"}}, streams)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec returned %v, want the cancelled context", err)
	}
}

// keyboard blocks until the test releases it, so the input ends after the command already has.
type keyboard struct {
	release <-chan struct{}
}

func (k keyboard) Read([]byte) (int, error) {
	<-k.release

	return 0, io.EOF
}

// A command that exits first takes the session with it, and the message saying the input ended then
// has nowhere to go. That is how every exec ends, and no warning belongs to it.
func TestExecSaysNothingWhenTheCommandEndedFirst(t *testing.T) {
	daemon := &execDaemon{t: t, execID: "1a2b3c4d5e6f7a8b", exit: &api.ExitMessage{}, skipInput: true}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	release := make(chan struct{})
	warnings := make(chan string, 4)

	streams := client.ExecStreams{
		Stdin:  keyboard{release: release},
		Stdout: io.Discard,
		Warn:   func(message string) { warnings <- message },
	}

	status, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, streams)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if status.Code != 0 {
		t.Errorf("exit code = %d, want 0", status.Code)
	}

	// The exec is over and the session with it, so the keyboard ends into nothing.
	close(release)

	select {
	case message := <-warnings:
		t.Errorf("the client warned %q about a command that had already exited", message)
	case <-time.After(warnBudget):
	}
}

func TestResizeExecPostsTheWindow(t *testing.T) {
	var asked string
	var body sandbox.TerminalSize

	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode the size: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.ResizeExec(t.Context(), "sandbox1", "1a2b3c4d5e6f7a8b", sandbox.TerminalSize{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("ResizeExec: %v", err)
	}

	if asked != "POST /v0/sandboxes/sandbox1/exec/1a2b3c4d5e6f7a8b/resize" {
		t.Errorf("the client asked %q", asked)
	}
	if body != (sandbox.TerminalSize{Rows: 24, Cols: 80}) {
		t.Errorf("the client sent %+v, want 24 by 80", body)
	}
}

func TestLogsWritesWhatTheDaemonAnswers(t *testing.T) {
	var asked string

	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := io.WriteString(w, "hello\nworld\n"); err != nil {
			t.Errorf("write the output: %v", err)
		}
	})

	var out bytes.Buffer
	if err := c.Logs(t.Context(), "sandbox1", false, &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if out.String() != "hello\nworld\n" {
		t.Errorf("Logs wrote %q", out.String())
	}
	if asked != "/v0/sandboxes/sandbox1/logs" {
		t.Errorf("the client asked %q", asked)
	}
}

func TestLogsReportsAnIDTheDaemonDoesNotHold(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found","code":"not_found"}`))

	var out bytes.Buffer

	for _, follow := range []bool{false, true} {
		err := c.Logs(t.Context(), "ghost", follow, &out)

		var missing *client.NotFoundError
		if !errors.As(err, &missing) || missing.Ref != "ghost" {
			t.Fatalf("Logs with follow=%v returned %v, want no sandbox ghost", follow, err)
		}
	}
}

// message is one thing a follow says: a binary message of a stream, or a text one.
type message struct {
	stream  byte
	text    bool
	payload string
}

// followDaemon answers a follow the way the daemon does: the 101, the messages, and a close that says why.
type followDaemon struct {
	t        *testing.T
	messages []message
	code     websocket.StatusCode
	reason   string

	asked string
}

func (d *followDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.asked = r.URL.RequestURI()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		d.t.Errorf("accept the follow: %v", err)

		return
	}
	defer conn.CloseNow()

	for _, m := range d.messages {
		if m.text {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(m.payload)); err != nil {
				d.t.Errorf("send a text message: %v", err)
			}

			continue
		}

		if err := api.Send(context.Background(), conn, m.stream, []byte(m.payload)); err != nil {
			d.t.Errorf("send a message of stream %d: %v", m.stream, err)
		}
	}

	if err := conn.Close(d.code, d.reason); err != nil {
		d.t.Errorf("close the follow: %v", err)
	}
}

func TestLogsFollowWritesEveryMessageUntilTheEnd(t *testing.T) {
	daemon := &followDaemon{t: t, messages: []message{
		{stream: api.StreamStdout, payload: "hello\n"},
		{stream: api.StreamStdout, payload: "world\n"},
		{stream: api.StreamExit, payload: `{"reason":"stopped"}`},
	}, code: websocket.StatusNormalClosure}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	var out bytes.Buffer
	if err := c.Logs(t.Context(), "sandbox1", true, &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if out.String() != "hello\nworld\n" {
		t.Errorf("the follow wrote %q", out.String())
	}
	if daemon.asked != "/v0/sandboxes/sandbox1/logs?follow=true" {
		t.Errorf("the client asked %q", daemon.asked)
	}
}

func TestLogsFollowReportsAFailureOfTheFollow(t *testing.T) {
	daemon := &followDaemon{t: t, messages: []message{
		{stream: api.StreamFailure, payload: `{"error":"read the output: permission denied","code":"internal"}`},
	}, code: websocket.StatusNormalClosure}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	var out bytes.Buffer

	err := c.Logs(t.Context(), "sandbox1", true, &out)

	var failure *client.APIError
	if !errors.As(err, &failure) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Logs returned %v, want the daemon's failure", err)
	}
}

func TestFollowEgressLogPrintsEveryRecordAndWhyItEnded(t *testing.T) {
	daemon := &followDaemon{t: t, messages: []message{
		{text: true, payload: `{"rule":"1"}`},
		{text: true, payload: `{"rule":"2"}`},
		{text: true, payload: `{"rule":"3"}`},
	}, code: websocket.StatusNormalClosure, reason: "the sandbox was removed"}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	var out, errOut bytes.Buffer
	if err := c.FollowEgressLog(t.Context(), "sandbox1", &out, &errOut); err != nil {
		t.Fatalf("FollowEgressLog: %v", err)
	}

	if want := `{"rule":"1"}` + "\n" + `{"rule":"2"}` + "\n" + `{"rule":"3"}` + "\n"; out.String() != want {
		t.Errorf("the follow printed %q", out.String())
	}
	if !strings.Contains(errOut.String(), "the sandbox was removed") {
		t.Errorf("the follow said %q about why it ended", errOut.String())
	}
	if daemon.asked != "/v0/sandboxes/sandbox1/egress-log?follow=true" {
		t.Errorf("the client asked %q", daemon.asked)
	}
}

// A failure of the follow itself is not a removed sandbox, and it must reach the operator as one.
func TestFollowEgressLogReportsAFailureOfTheFollow(t *testing.T) {
	daemon := &followDaemon{t: t, code: websocket.StatusInternalError, reason: "read the log: permission denied"}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	var out, errOut bytes.Buffer

	err := c.FollowEgressLog(t.Context(), "sandbox1", &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("FollowEgressLog returned %v", err)
	}
}

func TestFollowEgressLogReportsAnIDTheDaemonDoesNotHold(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found","code":"not_found"}`))

	var out, errOut bytes.Buffer

	err := c.FollowEgressLog(t.Context(), "ghost", &out, &errOut)

	var missing *client.NotFoundError
	if !errors.As(err, &missing) || missing.Ref != "ghost" {
		t.Fatalf("FollowEgressLog returned %v, want no sandbox ghost", err)
	}
}
