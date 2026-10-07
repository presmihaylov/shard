package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

// web is the process the fake answers for, as a run named it and the sandbox's supervisor reported it.
func web() models.Process {
	return models.Process{
		Name:    "web",
		Command: []string{"/bin/sleep", "600"},
		Restart: models.RestartSpec{Policy: models.RestartAlways},
		Status:  models.ProcessStatus{State: models.ProcessRunning, Restarts: 1},
	}
}

func decodeProcess(t *testing.T, body map[string]any) models.Process {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	var p models.Process
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("the body %s is no Process: %v", raw, err)
	}

	return p
}

func TestRunAnswers201WithTheProcess(t *testing.T) {
	s := seed(t)
	s.verbs.process = web()

	body := `{"name":"web","command":["/bin/sleep","600"],"env":["A=1"],"workdir":"/srv","user":"app","restart":{"policy":"always"}}`
	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/processes", body)
	if status != http.StatusCreated {
		t.Fatalf("run answered %d %v, want 201", status, got)
	}
	if p := decodeProcess(t, got); !reflect.DeepEqual(p, s.verbs.process) {
		t.Errorf("run answered %+v, want %+v", p, s.verbs.process)
	}

	var want sandbox.RunRequest
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.verbs.ran, want) || s.verbs.ref != s.running.ID {
		t.Errorf("the orchestrator ran %+v on %q, want %+v on %s", s.verbs.ran, s.verbs.ref, want, s.running.ID)
	}
}

func TestListAndGetAnswerTheProcesses(t *testing.T) {
	s := seed(t)
	s.verbs.process = web()
	s.verbs.processes = []models.Process{web()}

	status, got := get(t, s.server, "/v0/sandboxes/"+s.running.ID+"/processes")
	listed, ok := got["processes"].([]any)
	if status != http.StatusOK || !ok || len(listed) != 1 {
		t.Fatalf("the list answered %d %v, want 200 with one process", status, got)
	}

	status, got = get(t, s.server, "/v0/sandboxes/"+s.running.ID+"/processes/web")
	if status != http.StatusOK || s.verbs.named != "web" {
		t.Fatalf("the get answered %d %v for %q, want 200 for web", status, got, s.verbs.named)
	}
	if p := decodeProcess(t, got); !reflect.DeepEqual(p, s.verbs.process) {
		t.Errorf("the get answered %+v, want %+v", p, s.verbs.process)
	}
}

// A sandbox with no process answers an empty list, never null, so a client can range over it.
func TestListAnswersAnEmptyListForNoProcess(t *testing.T) {
	s := seed(t)

	status, got := get(t, s.server, "/v0/sandboxes/"+s.running.ID+"/processes")
	if listed, ok := got["processes"].([]any); status != http.StatusOK || !ok || len(listed) != 0 {
		t.Errorf("the list answered %d %v, want 200 with an empty list", status, got)
	}
}

func TestKillPassesTheForceAndAnswersTheProcess(t *testing.T) {
	for body, force := range map[string]bool{"": false, `{}`: false, `{"force":true}`: true} {
		s := seed(t)
		s.verbs.process = web()
		s.verbs.process.Killed, s.verbs.process.Status.State = true, models.ProcessKilled

		status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/processes/web/kill", body)
		if status != http.StatusOK {
			t.Fatalf("kill with %q answered %d %v, want 200", body, status, got)
		}
		if p := decodeProcess(t, got); p.Status.State != models.ProcessKilled || !p.Killed {
			t.Errorf("kill with %q answered %+v, want the killed process", body, p)
		}
		if s.verbs.named != "web" || s.verbs.force != force {
			t.Errorf("kill with %q ended %q with force %v, want web with force %v", body, s.verbs.named, s.verbs.force, force)
		}
	}
}

// Both the read and the follow name the process the path names, so one process's output never reads as another's.
func TestLogsReadTheNamedProcess(t *testing.T) {
	for _, path := range []string{"/processes/web/logs", "/processes/web/logs?follow=true"} {
		s := seed(t)
		s.verbs.lines = []string{"up\n"}

		st := follow(t, s, "/v0/sandboxes/"+s.running.ID+path)
		body, err := io.ReadAll(st.body)
		if err != nil || st.status != http.StatusOK || string(body) != "up\n" {
			t.Fatalf("GET %s answered %d %q, %v, want 200 with the output", path, st.status, body, err)
		}
		if s.verbs.named != "web" {
			t.Errorf("GET %s read the process %q, want web", path, s.verbs.named)
		}
	}
}

func TestAttachStreamsTheOutputThenTheEndedProcess(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"first run\n", "last run\n"}
	s.verbs.process = web()
	s.verbs.process.Status = models.ProcessStatus{State: models.ProcessExited, Restarts: 2, Exit: &models.ExitStatus{Code: 4}}

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/processes/web/attach")

	var out strings.Builder
	for {
		stream, payload, err := api.Receive(t.Context(), conn)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if stream == api.StreamStdout {
			out.Write(payload)

			continue
		}
		if stream != api.StreamExit {
			t.Fatalf("the daemon sent stream %d %q, want the ended process", stream, payload)
		}

		var p models.Process
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("the end carried %q: %v", payload, err)
		}
		if !reflect.DeepEqual(p, s.verbs.process) {
			t.Errorf("the end said %+v, want %+v", p, s.verbs.process)
		}

		break
	}

	if out.String() != "first run\nlast run\n" || s.verbs.named != "web" {
		t.Errorf("the attach to %q sent %q, want every line of web once", s.verbs.named, out.String())
	}
	if status := closed(t, conn); status != websocket.StatusNormalClosure {
		t.Errorf("the attach closed with %v, want a normal closure", status)
	}
}

// A script that wants the end alone sends a plain GET and reads the Process.
func TestAPlainAttachAnswersTheEndedProcess(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"never sent\n"}
	s.verbs.process = web()
	s.verbs.process.Status = models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{Code: 137, Signal: 9}}

	status, got := get(t, s.server, "/v0/sandboxes/"+s.running.ID+"/processes/web/attach")
	if status != http.StatusOK {
		t.Fatalf("the plain attach answered %d %v, want 200", status, got)
	}
	if p := decodeProcess(t, got); !reflect.DeepEqual(p, s.verbs.process) {
		t.Errorf("the plain attach answered %+v, want %+v", p, s.verbs.process)
	}
}

// Every refusal of a process verb names its code: a name the sandbox never ran is a 404, the rest a 409, and the attach says it before the 101.
func TestProcessRefusalsNameTheirCode(t *testing.T) {
	for _, c := range []struct {
		code         models.Code
		status       int
		method, path string
		body         string
	}{
		{models.CodeNoProcess, http.StatusNotFound, http.MethodGet, "/processes/web", ""},
		{models.CodeNoProcess, http.StatusNotFound, http.MethodPost, "/processes/web/kill", ""},
		{models.CodeNoProcess, http.StatusNotFound, http.MethodGet, "/processes/web/logs", ""},
		{models.CodeNoProcess, http.StatusNotFound, http.MethodGet, "/processes/web/attach", ""},
		{models.CodeProcessLimit, http.StatusConflict, http.MethodPost, "/processes", `{"command":["true"]}`},
		{models.CodeNameTaken, http.StatusConflict, http.MethodPost, "/processes", `{"name":"web","command":["true"]}`},
	} {
		s := seed(t)
		s.verbs.err = &sandbox.StateError{Sandbox: s.running.ID, State: models.StateRunning, Fix: "the fix", Code: c.code}

		status, body := send(t, s.server, c.method, "/v0/sandboxes/"+s.running.ID+c.path, c.body)
		if status != c.status || errorOf(t, body).code != string(c.code) {
			t.Errorf("%s %s answered %d %v, want %d %s", c.method, c.path, status, body, c.status, c.code)
		}
	}
}

func TestAttachRefusesAMissingProcessBeforeTheUpgrade(t *testing.T) {
	s := seed(t)
	s.verbs.err = &sandbox.StateError{Sandbox: s.running.ID, State: models.StateRunning, Fix: "it has no process web", Code: models.CodeNoProcess}

	_, resp, err := dial(t, s, "/v0/sandboxes/"+s.running.ID+"/processes/web/attach")
	if err == nil {
		t.Fatal("the attach upgraded, want a refusal")
	}

	status, body := decodeRefusal(t, resp)
	if status != http.StatusNotFound || errorOf(t, body).code != string(models.CodeNoProcess) {
		t.Errorf("the attach answered %d %v, want 404 %s", status, body, models.CodeNoProcess)
	}
}
