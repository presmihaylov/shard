package client_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

// shortRoot skips t.TempDir, whose path carries the test name past the 104 bytes a macOS socket path allows.
func shortRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "shard") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})

	return root
}

// serve answers on the socket under root with handler, the way the daemon does, and returns the client for it.
func serve(t *testing.T, root string, handler http.HandlerFunc) *client.Client {
	t.Helper()

	listener, err := net.Listen("unix", filepath.Join(root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return client.New(root)
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestVersionReadsWhatTheDaemonReports(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusOK, `{"version":"v-test"}`))

	got, err := c.Version(t.Context())
	if err != nil || got.Version != "v-test" {
		t.Errorf("Version = %+v, %v; want v-test", got, err)
	}
}

func TestListSandboxesAsksForAllOnlyWhenTold(t *testing.T) {
	var asked []string

	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		answer(http.StatusOK, `{"sandboxes":[{"id":"up-1","state":"running"}],"warnings":["decode sandbox.json of bad-2"]}`)(w, r)
	})

	got, err := c.ListSandboxes(t.Context(), false)
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(got.Sandboxes) != 1 || got.Sandboxes[0].ID != "up-1" || got.Sandboxes[0].State != models.StateRunning {
		t.Errorf("the list holds %+v, want up-1 running", got.Sandboxes)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "bad-2") {
		t.Errorf("the warnings are %v, want the one naming bad-2", got.Warnings)
	}

	if _, err := c.ListSandboxes(t.Context(), true); err != nil {
		t.Fatalf("ListSandboxes with all: %v", err)
	}

	if want := []string{"/v0/sandboxes", "/v0/sandboxes?all=true"}; strings.Join(asked, " ") != strings.Join(want, " ") {
		t.Errorf("the client asked %v, want %v", asked, want)
	}
}

func TestGetSandboxReadsTheRecordAndItsEgress(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/sandboxes/web" {
			answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"no route"}}`)(w, r)

			return
		}
		answer(http.StatusOK, `{"id":"up-1","name":"web","state":"running","policy":"deny-all","egress":{"policy":"deny-all","rules":[]}}`)(w, r)
	})

	got, err := c.GetSandbox(t.Context(), "web")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if got.ID != "up-1" || got.Egress == nil || got.Egress.Policy != "deny-all" {
		t.Errorf("GetSandbox = %+v, want up-1 with its egress", got)
	}
}

// A remote client reads the same public routes through the front, the wait included.
func TestARemoteClientReadsThePublicSandboxRoutes(t *testing.T) {
	asked := make(chan string, 3)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL.RequestURI()
		if r.URL.Path == "/v0/sandboxes" {
			answer(http.StatusOK, `{"sandboxes":[{"id":"up-1","state":"running"}]}`)(w, r)

			return
		}
		answer(http.StatusOK, `{"id":"up-1","state":"running"}`)(w, r)
	}))
	t.Cleanup(server.Close)

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	c, err := client.NewRemote(server.URL, "front-token-value", ca)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	if _, err := c.ListSandboxes(t.Context(), false); err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if _, err := c.GetSandbox(t.Context(), "web"); err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if _, err := c.WaitSandbox(t.Context(), "web"); err != nil {
		t.Fatalf("WaitSandbox: %v", err)
	}

	for _, want := range []string{"/v0/sandboxes", "/v0/sandboxes/web", "/v0/sandboxes/web?wait=true"} {
		if got := <-asked; got != want {
			t.Errorf("the remote client asked %s, want %s", got, want)
		}
	}
}

func TestGetSandboxTurnsNotFoundIntoItsOwnError(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"sandbox ghost: sandbox not found"}}`))

	_, err := c.GetSandbox(t.Context(), "ghost")

	var missing *client.NotFoundError
	if !errors.As(err, &missing) || missing.Ref != "ghost" {
		t.Fatalf("GetSandbox = %v, want a NotFoundError for ghost", err)
	}
	if err.Error() != "no sandbox ghost" {
		t.Errorf("the error reads %q, want 'no sandbox ghost'", err.Error())
	}
}

func TestAnyOtherStatusCarriesTheDaemonsMessage(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusInternalServerError, `{"error":{"code":"internal","message":"read the tree: permission denied"}}`))

	_, err := c.ListSandboxes(t.Context(), false)
	if err == nil || err.Error() != "read the tree: permission denied" {
		t.Errorf("ListSandboxes = %v, want the daemon's message as it came", err)
	}

	// The flat shape of an older daemon is not read: the message and the code are quoted like any other body.
	for _, body := range []string{`not json`, `{"error":"read the tree: permission denied","code":"internal"}`} {
		c = serve(t, shortRoot(t), answer(http.StatusBadGateway, body))

		_, err = c.Version(t.Context())
		if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), fmt.Sprintf("%q", body)) {
			t.Errorf("Version = %v, want the status and the body %q quoted", err, body)
		}
	}
}

func TestNoDaemonIsOneConnectLine(t *testing.T) {
	root := shortRoot(t)
	c := client.New(root)

	_, err := c.Version(t.Context())

	var connect *client.ConnectError
	if !errors.As(err, &connect) {
		t.Fatalf("Version = %v, want a ConnectError", err)
	}
	want := "cannot connect to shard daemon at " + filepath.Join(root, api.SocketFile) + ": is it running? shard --root " + root + " daemon"
	if err.Error() != want {
		t.Errorf("the error reads %q, want %q", err.Error(), want)
	}
}

func TestADaemonThatNeverAnswersIsCutByTheDeadline(t *testing.T) {
	root := shortRoot(t)
	c := serve(t, root, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c.Timeout = 100 * time.Millisecond

	start := time.Now()
	_, err := c.ListSandboxes(t.Context(), false)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("ListSandboxes took %s to give up, want the deadline", took)
	}
	want := "GET /v0/sandboxes on " + filepath.Join(root, api.SocketFile) + ": no answer within 100ms"
	if err == nil || err.Error() != want {
		t.Errorf("ListSandboxes = %v, want %q", err, want)
	}

	// The caller's own deadline is reported as what it is, not as the client's.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	c.Timeout = time.Minute
	if _, err := c.ListSandboxes(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ListSandboxes under the caller's deadline = %v, want context.DeadlineExceeded", err)
	}
}

func TestEgressLogWritesEveryRecordInOneWrite(t *testing.T) {
	body := `[{"time":"2026-01-01T00:00:00Z","source":"host","verdict":"deny","rule":"local"},` +
		`{"time":"2026-01-01T00:00:01Z","source":"proxy","verdict":"allow","rule":"r-1"}]`
	c := serve(t, shortRoot(t), answer(http.StatusOK, body))

	var out countingWriter
	var errOut bytes.Buffer
	if err := c.EgressLog(t.Context(), "web", &out, &errOut); err != nil {
		t.Fatalf("EgressLog: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("EgressLog wrote %q on stderr for a log it printed whole", errOut.String())
	}

	// One write, so grep -q closing the pipe early never leaves the CLI a partial write to SIGPIPE on.
	if out.writes != 1 {
		t.Errorf("EgressLog made %d writes, want exactly 1", out.writes)
	}

	want := `{"time":"2026-01-01T00:00:00Z","source":"host","verdict":"deny","rule":"local"}` + "\n" +
		`{"time":"2026-01-01T00:00:01Z","source":"proxy","verdict":"allow","rule":"r-1"}` + "\n"
	if got := out.buf.String(); got != want {
		t.Errorf("EgressLog wrote\n%q\nwant\n%q", got, want)
	}
}

func TestEgressLogSaysOnStderrHowManyOlderRecordsTheDaemonLeftOut(t *testing.T) {
	c := serve(t, shortRoot(t), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(api.EgressCutHeader, "12345")
		answer(http.StatusOK, `[{"time":"2026-01-01T00:00:00Z","source":"host","verdict":"deny","rule":"local"}]`)(w, nil)
	})

	var out, errOut bytes.Buffer
	if err := c.EgressLog(t.Context(), "web", &out, &errOut); err != nil {
		t.Fatalf("EgressLog: %v", err)
	}

	if got := errOut.String(); !strings.Contains(got, "12345") || !strings.Contains(got, "web") {
		t.Errorf("EgressLog wrote %q on stderr, want the 12345 older records of web named", got)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Errorf("EgressLog wrote %q, want the one record and nothing else", out.String())
	}
}

// countingWriter counts Write calls so a test can prove EgressLog emits the whole log in one write.
type countingWriter struct {
	writes int
	buf    bytes.Buffer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++

	return w.buf.Write(p)
}
