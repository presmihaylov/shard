package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestAttachStreamsTheAppOutputThenHowItEnded(t *testing.T) {
	s := seed(t)
	s.verbs.lines = []string{"first run\n", "last run\n"}
	s.verbs.appExit = models.AppExit{Code: 4, Restarts: 2}

	conn := open(t, s, "/v0/sandboxes/"+s.running.ID+"/attach")

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
			t.Fatalf("the daemon sent stream %d %q, want the exit", stream, payload)
		}

		var exit models.AppExit
		if err := json.Unmarshal(payload, &exit); err != nil {
			t.Fatalf("the exit carried %q: %v", payload, err)
		}
		if exit != s.verbs.appExit {
			t.Errorf("the exit said %+v, want %+v", exit, s.verbs.appExit)
		}

		break
	}

	if out.String() != "first run\nlast run\n" {
		t.Errorf("the attach sent %q, want every line once", out.String())
	}
	if status := closed(t, conn); status != websocket.StatusNormalClosure {
		t.Errorf("the attach closed with %v, want a normal closure", status)
	}
}

// A sandbox that create made has no app, and the refusal is a 409 before the 101 like every other route's.
func TestAttachRefusesASandboxWithNoAppBeforeTheUpgrade(t *testing.T) {
	s := seed(t)
	s.verbs.err = &sandbox.StateError{ID: s.running.ID, State: models.StateRunning, Fix: "shard run starts a sandbox with an app", Code: models.CodeNoApp}

	_, resp, err := dial(t, s, "/v0/sandboxes/"+s.running.ID+"/attach")
	if err == nil {
		t.Fatal("the attach upgraded, want a refusal")
	}

	status, body := decodeRefusal(t, resp)
	if status != http.StatusConflict || errorOf(t, body).code != string(models.CodeNoApp) {
		t.Errorf("the attach answered %d %v, want 409 %s", status, body, models.CodeNoApp)
	}
}

// A script that wants the code alone sends a plain GET; a query it adds changes nothing.
func TestAPlainAttachAnswersHowTheAppEnded(t *testing.T) {
	for _, path := range []string{"/attach", "/attach?wait=true"} {
		t.Run(path, func(t *testing.T) {
			s := seed(t)
			s.verbs.appExit = models.AppExit{Code: 137, Signal: 9, Restarts: 1}

			status, body := get(t, s.server, "/v0/sandboxes/"+s.running.ID+path)
			if status != http.StatusOK {
				t.Fatalf("GET %s answered %d %v, want 200", path, status, body)
			}
			if body["code"] != float64(137) || body["signal"] != float64(9) || body["restarts"] != float64(1) {
				t.Errorf("GET %s answered %v, want the code, the signal and the restarts", path, body)
			}
			if s.verbs.waited != s.running.ID {
				t.Errorf("GET %s waited on %q, want %q", path, s.verbs.waited, s.running.ID)
			}
		})
	}
}

func TestAppStopPassesTheForce(t *testing.T) {
	for body, force := range map[string]bool{"": false, `{}`: false, `{"force":true}`: true} {
		s := seed(t)

		status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/app/stop", body)
		if status != http.StatusNoContent {
			t.Fatalf("app/stop with %q answered %d %v, want 204", body, status, got)
		}
		if !s.verbs.stoppedApp || s.verbs.force != force {
			t.Errorf("app/stop with %q stopped %v with force %v, want force %v", body, s.verbs.stoppedApp, s.verbs.force, force)
		}
	}
}

func TestAppStopRefusesAnAppThatEnded(t *testing.T) {
	s := seed(t)
	s.verbs.err = &sandbox.StateError{ID: s.running.ID, State: models.StateRunning, Fix: "the app already ended", Code: models.CodeAppEnded}

	status, body := send(t, s.server, http.MethodPost, "/v0/sandboxes/"+s.running.ID+"/app/stop", `{"force":true}`)
	if status != http.StatusConflict || errorOf(t, body).code != string(models.CodeAppEnded) {
		t.Errorf("app/stop answered %d %v, want 409 %s", status, body, models.CodeAppEnded)
	}
}
