package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

// serveDaemon answers on the socket under the app's root over its fakes, the way the daemon does over the stores.
func serveDaemon(t *testing.T, d *deps) {
	t.Helper()

	repo, err := d.repo()
	if err != nil {
		t.Fatalf("repo: %v", err)
	}

	enforcer, err := d.egress()
	if err != nil {
		t.Fatalf("egress: %v", err)
	}

	listener, err := net.Listen("unix", filepath.Join(d.app.Root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := httptest.NewUnstartedServer(api.NewHandler("v-daemon", repo, enforcer, io.Discard))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
}

// newClientApp is newLifecycleApp with a daemon up on its root, for the verbs that only speak to one.
func newClientApp(t *testing.T, out *bytes.Buffer, sb models.Sandbox) (App, *deps) {
	t.Helper()

	app, d := newLifecycleApp(t, out, &recorder{}, sb)
	serveDaemon(t, d)

	return app, d
}

func TestVersionPrintsBothLines(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, models.Sandbox{})

	if err := app.Run(t.Context(), []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "client test\ndaemon v-daemon" {
		t.Errorf("version printed %q, want the client line and the daemon line", got)
	}
}

func TestVersionFlagPrintsTheClientLineWithNoDaemon(t *testing.T) {
	var out bytes.Buffer

	app := App{Version: "test", Root: shortRoot(t), Out: &out}

	if err := app.Run(t.Context(), []string{"--version"}); err != nil {
		t.Fatalf("--version with no daemon failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "client test" {
		t.Errorf("--version printed %q, want the client line alone", got)
	}
}

func TestLsFailsWhenTheDaemonNeverAnswers(t *testing.T) {
	var out bytes.Buffer

	root := shortRoot(t)
	app := App{Version: "test", Root: root, Out: &out, clientTimeout: 100 * time.Millisecond}

	listener, err := net.Listen("unix", filepath.Join(root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	start := time.Now()
	err = app.Run(t.Context(), []string{"ls"})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("ls took %s to give up, want the 100ms deadline", took)
	}
	if want := "GET /v0/sandboxes on " + filepath.Join(root, api.SocketFile) + ": no answer within 100ms"; err == nil || err.Error() != want {
		t.Errorf("ls returned %v, want %q", err, want)
	}
}

func TestVersionWithNoDaemonPrintsTheClientLineAndFails(t *testing.T) {
	var out bytes.Buffer

	app := App{Version: "test", Root: shortRoot(t), Out: &out}

	err := app.Run(t.Context(), []string{"version"})

	var connect *client.ConnectError
	if !errors.As(err, &connect) {
		t.Fatalf("version with no daemon returned %v, want the connect error", err)
	}
	if got := strings.TrimSpace(out.String()); got != "client test" {
		t.Errorf("version printed %q, want the client line alone", got)
	}
}

func TestLsWithNoDaemonFailsFast(t *testing.T) {
	var out bytes.Buffer

	root := shortRoot(t)
	app := App{Version: "test", Root: root, Out: &out}

	err := app.Run(t.Context(), []string{"ls"})
	if want := "cannot connect to shard daemon at " + filepath.Join(root, api.SocketFile) + ": is it running? systemctl status shard"; err == nil || err.Error() != want {
		t.Errorf("ls with no daemon returned %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Errorf("ls printed %q before it failed", out.String())
	}
}
