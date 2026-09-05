package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// fakeLifecycle answers every verb with err, or with a record named after the reference it was given.
type fakeLifecycle struct {
	err error

	created sandbox.CreateRequest
	ref     string
	grace   time.Duration
	force   bool

	// exec is the request the client sent, and input what it typed at the command.
	exec   sandbox.ExecRequest
	input  string
	execID string
	// out and errOut are what the command writes on each stream, and exit how it ended.
	out    string
	errOut string
	exit   models.ExitStatus
	// execErr is how the command failed, apart from err, which is a refusal before the 101.
	execErr     error
	resizedExec string
	size        sandbox.TerminalSize

	// lines is what the output holds, and stops is what ends a follow.
	lines    []string
	followed bool
	stops    chan struct{}
}

func (f *fakeLifecycle) Create(_ context.Context, req sandbox.CreateRequest) (models.Sandbox, error) {
	f.created = req

	return models.Sandbox{ID: "sandbox1", Name: req.Name, Image: req.Image, State: models.StateRunning}, f.err
}

func (f *fakeLifecycle) Start(_ context.Context, ref string) (models.Sandbox, error) {
	f.ref = ref

	return models.Sandbox{ID: ref, State: models.StateRunning}, f.err
}

func (f *fakeLifecycle) Stop(_ context.Context, ref string, grace time.Duration) (models.Sandbox, error) {
	f.ref, f.grace = ref, grace

	return models.Sandbox{ID: ref, State: models.StateStopped}, f.err
}

func (f *fakeLifecycle) Remove(_ context.Context, ref string, force bool, grace time.Duration) error {
	f.ref, f.force, f.grace = ref, force, grace

	return f.err
}

// Exec answers the client the way the orchestrator does: it names the exec, writes, and then exits.
func (f *fakeLifecycle) Exec(_ context.Context, ref string, req sandbox.ExecRequest, streams sandbox.Streams) (models.ExitStatus, error) {
	f.ref, f.exec = ref, req

	if f.err != nil {
		return models.ExitStatus{}, f.err
	}

	if streams.Started != nil {
		if err := streams.Started(f.execID); err != nil {
			return models.ExitStatus{}, err
		}
	}

	// A command that never ran reads nothing, the way the substrate answers one it could not start.
	if f.execErr != nil {
		return models.ExitStatus{}, f.execErr
	}

	if f.out != "" {
		if _, err := streams.Stdout.Write([]byte(f.out)); err != nil {
			return models.ExitStatus{}, err
		}
	}
	if f.errOut != "" {
		if _, err := streams.Stderr.Write([]byte(f.errOut)); err != nil {
			return models.ExitStatus{}, err
		}
	}

	if streams.Stdin != nil {
		read, err := io.ReadAll(streams.Stdin)
		if err != nil {
			return models.ExitStatus{}, err
		}
		f.input = string(read)
	}

	return f.exit, nil
}

func (f *fakeLifecycle) ResizeExec(_ context.Context, ref, execID string, size sandbox.TerminalSize) error {
	f.ref, f.resizedExec, f.size = ref, execID, size

	return f.err
}

func (f *fakeLifecycle) Logs(ctx context.Context, ref string, follow bool, w io.Writer) error {
	f.ref, f.followed = ref, follow

	if f.err != nil {
		return f.err
	}

	for _, line := range f.lines {
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}

	// A follow ends when the sandbox stops; this one ends when the test says the sandbox has.
	if follow && f.stops != nil {
		select {
		case <-f.stops:
		case <-ctx.Done():
		}
	}

	return nil
}

// send answers with the status and the decoded body, or a nil body on a 204.
func send(t *testing.T, server *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		raw, err := io.ReadAll(resp.Body)
		if err != nil || len(raw) != 0 {
			t.Errorf("%s %s answered 204 with a body %q: %v", method, path, raw, err)
		}

		return resp.StatusCode, nil
	}

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s %s: decode the body: %v", method, path, err)
	}

	return resp.StatusCode, decoded
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestCreateAnswers201WithTheRecord(t *testing.T) {
	s := seed(t)

	body := `{"image":"alpine:3.20","name":"web","command":["sh","-c","sleep 600"],"env":["A=1"],"secrets":["TOKEN"],"policy":"locked","resources":{"memory_mib":512,"vcpus":2}}`
	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", body)
	if status != http.StatusCreated || got["id"] != "sandbox1" || got["state"] != "running" {
		t.Fatalf("POST /v0/sandboxes answered %d %v, want 201 with the record", status, got)
	}

	var want sandbox.CreateRequest
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustJSON(t, s.verbs.created), mustJSON(t, want)) {
		t.Errorf("the orchestrator got %+v, want %+v", s.verbs.created, want)
	}
}

func TestCreateIs400ForABodyItCannotDecode(t *testing.T) {
	s := seed(t)

	for name, body := range map[string]string{"not json": "{not json", "an unknown field": `{"image":"alpine","imagee":"x"}`} {
		status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", body)
		if status != http.StatusBadRequest || got["error"] == nil {
			t.Errorf("POST with %s answered %d %v, want 400", name, status, got)
		}
	}
	if s.verbs.created.Image != "" {
		t.Errorf("a body that did not decode still reached the orchestrator: %+v", s.verbs.created)
	}
}

// 400 for the request, 404 for the reference, 409 for the state, 500 for the host.
func TestTheStatusFollowsTheError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"a request error", &sandbox.RequestError{Err: errors.New("secret NOPE does not exist")}, http.StatusBadRequest, "secret NOPE"},
		{"a bad name", &sandboxstate.ValidationError{Reason: "the name is a slash"}, http.StatusBadRequest, "slash"},
		{"not found", fmt.Errorf("sandbox ghost: %w", sandboxstate.ErrNotFound), http.StatusNotFound, "ghost"},
		{"a state error", &sandbox.StateError{ID: "sandbox1", State: models.StateRunning, Fix: "stop it first with shard stop sandbox1, or pass --force"}, http.StatusConflict, "sandbox sandbox1 is running: stop it first with shard stop sandbox1, or pass --force"},
		{"anything else", errors.New("runsc: boom"), http.StatusInternalServerError, "boom"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := seed(t)
			s.verbs.err = c.err

			for _, route := range []struct{ method, path, body string }{
				{http.MethodPost, "/v0/sandboxes", `{"image":"alpine"}`},
				{http.MethodPost, "/v0/sandboxes/sandbox1/start", ""},
				{http.MethodPost, "/v0/sandboxes/sandbox1/stop", ""},
				{http.MethodDelete, "/v0/sandboxes/sandbox1", ""},
			} {
				status, got := send(t, s.server, route.method, route.path, route.body)
				if status != c.status || !strings.Contains(got["error"].(string), c.text) {
					t.Errorf("%s %s answered %d %v, want %d with %q", route.method, route.path, status, got, c.status, c.text)
				}
			}
		})
	}
}

func TestStartAnswersTheRecord(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/web/start", "")
	if status != http.StatusOK || got["id"] != "web" || got["state"] != "running" {
		t.Errorf("POST /v0/sandboxes/web/start answered %d %v, want 200 with the record", status, got)
	}
	if s.verbs.ref != "web" {
		t.Errorf("the orchestrator got the reference %q, want web", s.verbs.ref)
	}
}

func TestStopPassesTheGraceInSeconds(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/stop", `{"grace":2.5}`)
	if status != http.StatusOK || got["state"] != "stopped" {
		t.Fatalf("POST stop answered %d %v, want 200 with the record", status, got)
	}
	if s.verbs.grace != 2500*time.Millisecond {
		t.Errorf("the orchestrator got the grace %s, want 2.5s", s.verbs.grace)
	}

	send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/stop", "")
	if s.verbs.grace != sandbox.DefaultStopGrace {
		t.Errorf("an empty body gave the grace %s, want the default %s", s.verbs.grace, sandbox.DefaultStopGrace)
	}
}

func TestStopIs400ForANegativeGrace(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/stop", `{"grace":-1}`)
	if status != http.StatusBadRequest || !strings.Contains(got["error"].(string), "negative") {
		t.Errorf("a negative grace answered %d %v, want 400", status, got)
	}
	if s.verbs.ref != "" {
		t.Error("a negative grace still reached the orchestrator")
	}
}

func TestDeleteAnswers204AndPassesForceAndGrace(t *testing.T) {
	s := seed(t)

	status, _ := send(t, s.server, http.MethodDelete, "/v0/sandboxes/sandbox1?force=true&grace=3", "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE answered %d, want 204", status)
	}
	if s.verbs.ref != "sandbox1" || !s.verbs.force || s.verbs.grace != 3*time.Second {
		t.Errorf("the orchestrator got ref=%q force=%v grace=%s, want sandbox1 true 3s", s.verbs.ref, s.verbs.force, s.verbs.grace)
	}

	send(t, s.server, http.MethodDelete, "/v0/sandboxes/sandbox1", "")
	if s.verbs.force || s.verbs.grace != sandbox.DefaultStopGrace {
		t.Errorf("a bare delete gave force=%v grace=%s, want false and the default", s.verbs.force, s.verbs.grace)
	}
}

func TestDeleteIs400ForAQueryItCannotRead(t *testing.T) {
	s := seed(t)

	for _, query := range []string{"?force=yes", "?grace=soon", "?grace=-1"} {
		status, got := send(t, s.server, http.MethodDelete, "/v0/sandboxes/sandbox1"+query, "")
		if status != http.StatusBadRequest || got["error"] == nil {
			t.Errorf("DELETE %s answered %d %v, want 400", query, status, got)
		}
	}
	if s.verbs.ref != "" {
		t.Error("a query that did not parse still reached the orchestrator")
	}
}
