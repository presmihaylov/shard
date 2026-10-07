package client_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

const webProcess = `{"name":"web","command":["/bin/sleep","600"],"restart":{"policy":"always"},"status":{"state":"running","restarts":1}}`

func TestRunPostsTheRequestAndDecodesTheProcess(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusCreated, webProcess, &saw))

	req := sandbox.RunRequest{Name: "web", Command: []string{"/bin/sleep", "600"}, Env: []string{"A=1"}, Restart: &models.RestartSpec{Policy: models.RestartAlways}}

	p, err := c.Run(t.Context(), "sb", req)
	if err != nil || p.Name != "web" || p.Status.State != models.ProcessRunning || p.Status.Restarts != 1 {
		t.Fatalf("Run = %+v, %v; want web running after one restart", p, err)
	}
	if saw.method != http.MethodPost || saw.uri != "/v0/sandboxes/sb/processes" || saw.contentType != "application/json" {
		t.Errorf("the request was %s %s as %q, want POST /v0/sandboxes/sb/processes as JSON", saw.method, saw.uri, saw.contentType)
	}

	var got sandbox.RunRequest
	if err := json.Unmarshal(saw.body, &got); err != nil {
		t.Fatalf("the body %s is not a request: %v", saw.body, err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Errorf("the daemon got %+v, want %+v", got, req)
	}
}

func TestProcessesAndProcessReadTheProcessRoutes(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusOK, `{"processes":[`+webProcess+`]}`, &saw))

	procs, err := c.Processes(t.Context(), "sb")
	if err != nil || len(procs) != 1 || procs[0].Name != "web" {
		t.Fatalf("Processes = %+v, %v; want web alone", procs, err)
	}
	if saw.method != http.MethodGet || saw.uri != "/v0/sandboxes/sb/processes" {
		t.Errorf("the list was %s %s, want GET /v0/sandboxes/sb/processes", saw.method, saw.uri)
	}

	c = serve(t, shortRoot(t), echo(http.StatusOK, webProcess, &saw))

	p, err := c.Process(t.Context(), "sb", "web")
	if err != nil || p.Name != "web" {
		t.Fatalf("Process = %+v, %v; want web", p, err)
	}
	if saw.method != http.MethodGet || saw.uri != "/v0/sandboxes/sb/processes/web" {
		t.Errorf("the get was %s %s, want GET /v0/sandboxes/sb/processes/web", saw.method, saw.uri)
	}
}

func TestKillPostsTheForceOnlyWhenForced(t *testing.T) {
	for force, body := range map[bool]string{false: `{}`, true: `{"force":true}`} {
		var saw seen
		c := serve(t, shortRoot(t), echo(http.StatusOK, `{"name":"web","killed":true,"status":{"state":"killed","restarts":0}}`, &saw))

		p, err := c.Kill(t.Context(), "sb", "web", force)
		if err != nil || !p.Killed || p.Status.State != models.ProcessKilled {
			t.Fatalf("Kill force=%v = %+v, %v; want web killed", force, p, err)
		}
		if saw.method != http.MethodPost || saw.uri != "/v0/sandboxes/sb/processes/web/kill" || string(bytes.TrimSpace(saw.body)) != body {
			t.Errorf("the kill was %s %s with %s, want POST /v0/sandboxes/sb/processes/web/kill with %s", saw.method, saw.uri, saw.body, body)
		}
	}
}

func TestAttachProcessWritesTheOutputThenAnswersTheEndedProcess(t *testing.T) {
	daemon := &followDaemon{t: t, messages: []message{
		{stream: api.StreamStdout, payload: "first run\n"},
		{stream: api.StreamStdout, payload: "last run\n"},
		{stream: api.StreamExit, payload: `{"name":"web","status":{"state":"exited","restarts":2,"exit":{"code":4,"signal":0}}}`},
	}, code: websocket.StatusNormalClosure}
	c := serve(t, shortRoot(t), daemon.ServeHTTP)

	var out bytes.Buffer
	p, err := c.AttachProcess(t.Context(), "sb", "web", &out)
	if err != nil {
		t.Fatalf("AttachProcess: %v", err)
	}

	if out.String() != "first run\nlast run\n" {
		t.Errorf("the attach wrote %q", out.String())
	}
	if p.Status.State != models.ProcessExited || p.Status.Exit == nil || p.Status.Exit.Code != 4 || p.Status.Restarts != 2 {
		t.Errorf("the attach answered %+v, want web exited 4 after two restarts", p.Status)
	}
	if daemon.asked != "/v0/sandboxes/sb/processes/web/attach" {
		t.Errorf("the client asked %q", daemon.asked)
	}
}

func TestTheProcessVerbsTurnA404IntoNotFound(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"sandbox ghost not found"}}`))

	calls := map[string]func() error{
		"run": func() error {
			_, err := c.Run(t.Context(), "ghost", sandbox.RunRequest{Command: []string{"true"}})
			return err
		},
		"processes": func() error { _, err := c.Processes(t.Context(), "ghost"); return err },
		"process":   func() error { _, err := c.Process(t.Context(), "ghost", "web"); return err },
		"kill":      func() error { _, err := c.Kill(t.Context(), "ghost", "web", false); return err },
		"attach":    func() error { _, err := c.AttachProcess(t.Context(), "ghost", "web", nil); return err },
	}

	for verb, call := range calls {
		err := call()

		var missing *client.NotFoundError
		if !errors.As(err, &missing) || missing.Ref != "ghost" {
			t.Errorf("%s = %v, want a NotFoundError for ghost", verb, err)
		}
	}
}
