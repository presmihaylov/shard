package client_test

import (
	"context"
	"errors"
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
			answer(http.StatusNotFound, `{"error":"no route"}`)(w, r)

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

func TestGetSandboxTurnsA404IntoNotFound(t *testing.T) {
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":"sandbox ghost: sandbox not found"}`))

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
	c := serve(t, shortRoot(t), answer(http.StatusInternalServerError, `{"error":"read the tree: permission denied"}`))

	_, err := c.ListSandboxes(t.Context(), false)
	if err == nil || err.Error() != "read the tree: permission denied" {
		t.Errorf("ListSandboxes = %v, want the daemon's message as it came", err)
	}

	c = serve(t, shortRoot(t), answer(http.StatusBadGateway, `not json`))

	_, err = c.Version(t.Context())
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "not json") {
		t.Errorf("Version = %v, want the status and the body quoted", err)
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
	want := "cannot connect to shard daemon at " + filepath.Join(root, api.SocketFile) + ": is it running? systemctl status shard"
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
