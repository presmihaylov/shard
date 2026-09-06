package client_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// warnBudget is how long a warning nobody wants has to arrive before the test says it never came.
const warnBudget = 2 * time.Second

// execDaemon is a daemon that answers one exec: it takes the connection over and speaks frames.
type execDaemon struct {
	t      *testing.T
	execID string

	out     string
	errOut  string
	failure string
	exit    string
	// hangUp ends the connection with no exit frame, the way a daemon that died mid-exec does.
	hangUp bool
	// skipInput answers and hangs up without reading, the way a command that exited at once does.
	skipInput bool

	// req is what the client asked for, and input what it typed at the command.
	req   sandbox.ExecRequest
	input string
}

func (d *execDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := json.NewDecoder(r.Body).Decode(&d.req); err != nil {
		d.t.Errorf("decode the exec request: %v", err)

		return
	}

	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		d.t.Errorf("hijack: %v", err)

		return
	}
	defer conn.Close()

	answer := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n" + api.ExecIDHeader + ": " + d.execID + "\r\n\r\n"
	if _, err := buffered.WriteString(answer); err != nil {
		d.t.Errorf("answer the exec: %v", err)

		return
	}
	if err := buffered.Flush(); err != nil {
		d.t.Errorf("flush the answer: %v", err)

		return
	}

	if !d.skipInput {
		d.input = d.readInput(buffered.Reader)
	}

	if d.hangUp {
		return
	}

	frames := api.NewFrameWriter(conn)
	for _, frame := range []struct {
		stream  byte
		payload string
	}{{api.StreamStdout, d.out}, {api.StreamStderr, d.errOut}, {api.StreamError, d.failure}, {api.StreamExit, d.exit}} {
		if frame.payload == "" && frame.stream != api.StreamExit {
			continue
		}
		if err := frames.Write(frame.stream, []byte(frame.payload)); err != nil {
			d.t.Errorf("write a frame of stream %d: %v", frame.stream, err)
		}
	}
}

// readInput collects what the client typed until it says the keyboard has ended.
func (d *execDaemon) readInput(r io.Reader) string {
	var typed strings.Builder

	for {
		stream, payload, err := api.ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return typed.String()
		}
		if err != nil {
			d.t.Errorf("read a frame: %v", err)

			return typed.String()
		}

		switch stream {
		case api.StreamStdin:
			typed.Write(payload)
		case api.StreamStdinClose:
			return typed.String()
		default:
			d.t.Errorf("the client sent a frame of stream %d", stream)

			return typed.String()
		}
	}
}

func TestExecCarriesTheCommandAndReportsItsExitStatus(t *testing.T) {
	daemon := &execDaemon{t: t, execID: "1a2b3c4d5e6f7a8b", out: "hello\n", errOut: "careful\n", exit: "7"}
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
	if named != "1a2b3c4d5e6f7a8b" {
		t.Errorf("the exec was named %q", named)
	}
	if daemon.input != "typed\n" {
		t.Errorf("the daemon read %q, want typed", daemon.input)
	}
	if strings.Join(daemon.req.Command, " ") != "sh -c exit 7" || daemon.req.WorkDir != "/srv" {
		t.Errorf("the daemon was asked for %+v", daemon.req)
	}
}

// A command that never ran arrives as an error frame and an exit frame, and rebuilds as the typed error.
func TestExecReportsACommandThatNeverRan(t *testing.T) {
	daemon := &execDaemon{t: t, failure: "failed to load /bin/nope: no such file or directory", exit: "127"}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"/bin/nope"}}, client.ExecStreams{})

	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) {
		t.Fatalf("Exec returned %v, want a command that never ran", err)
	}
	if notStarted.Code != 127 {
		t.Errorf("code = %d, want 127", notStarted.Code)
	}
	if notStarted.Reason != daemon.failure {
		t.Errorf("reason = %q, want %q", notStarted.Reason, daemon.failure)
	}
	if notStarted.Sandbox != "sandbox1" {
		t.Errorf("sandbox = %q, want sandbox1", notStarted.Sandbox)
	}
}

// Nothing is on the wire before the command runs, so a refusal is still a status and a JSON body.
func TestExecReportsARefusalBeforeTheUpgrade(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusConflict, `{"error":"sandbox sandbox1 is stopped: start it again with shard start sandbox1"}`))

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})
	if err == nil || !strings.Contains(err.Error(), "shard start sandbox1") {
		t.Fatalf("Exec returned %v, want the daemon's refusal", err)
	}
}

func TestExecReportsAnIDTheDaemonDoesNotHold(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found"}`))

	_, err := c.Exec(t.Context(), "ghost", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})

	var missing *client.NotFoundError
	if !errors.As(err, &missing) || missing.Ref != "ghost" {
		t.Fatalf("Exec returned %v, want no sandbox ghost", err)
	}
}

// Every exec ends with an exit frame, so a connection that ends without one is a failure and not a zero.
func TestExecReportsAnExecThatEndedWithNoStatus(t *testing.T) {
	daemon := &execDaemon{t: t, hangUp: true}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	_, err := c.Exec(t.Context(), "sandbox1", sandbox.ExecRequest{Command: []string{"true"}}, client.ExecStreams{})
	if err == nil || !strings.Contains(err.Error(), "without an exit status") {
		t.Fatalf("Exec returned %v, want the missing exit status named", err)
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

// A command that exits first takes the connection with it, and the frame saying the input ended then
// has nowhere to go. That is how every exec ends, and no warning belongs to it.
func TestExecSaysNothingWhenTheCommandEndedFirst(t *testing.T) {
	daemon := &execDaemon{t: t, execID: "1a2b3c4d5e6f7a8b", exit: "0", skipInput: true}
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

	// The exec is over and the connection with it, so the keyboard ends into nothing.
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

func TestLogsWritesWhatTheDaemonStreams(t *testing.T) {
	var asked string

	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := io.WriteString(w, "hello\nworld\n"); err != nil {
			t.Errorf("write the output: %v", err)
		}
	})

	var out bytes.Buffer
	if err := c.Logs(t.Context(), "sandbox1", true, &out); err != nil {
		t.Fatalf("Logs: %v", err)
	}

	if out.String() != "hello\nworld\n" {
		t.Errorf("Logs wrote %q", out.String())
	}
	if asked != "/v0/sandboxes/sandbox1/logs?follow=true" {
		t.Errorf("the client asked %q", asked)
	}
}

func TestLogsReportsAnIDTheDaemonDoesNotHold(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found"}`))

	var out bytes.Buffer

	err := c.Logs(t.Context(), "ghost", false, &out)

	var missing *client.NotFoundError
	if !errors.As(err, &missing) || missing.Ref != "ghost" {
		t.Fatalf("Logs returned %v, want no sandbox ghost", err)
	}
}
