package cli

import (
	"bytes"
	"errors"
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

// newClientApp is newLifecycleApp for the verbs whose test reads no call order.
func newClientApp(t *testing.T, out *bytes.Buffer, sb models.Sandbox) (App, *fakeDaemon) {
	t.Helper()

	app, f := newLifecycleApp(t, out, &recorder{}, sb)

	return app, f
}

func TestVersionPrintsBothLines(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, models.Sandbox{})

	if err := app.Run(t.Context(), []string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}

	// A Mac binary adds a third line for the VM shim after these two.
	if got := out.String(); !strings.HasPrefix(got, "client test\ndaemon v-daemon\n") {
		t.Errorf("version printed %q, want the client line and the daemon line", got)
	}
}

func TestDaemonStatusPrintsOneFieldPerLine(t *testing.T) {
	var out bytes.Buffer

	app, f := newClientApp(t, &out, models.Sandbox{})
	f.providerSvc = &fakeLifecycleProvider{r: &recorder{}, noFork: true}

	if err := app.Run(t.Context(), []string{"daemon", "status"}); err != nil {
		t.Fatalf("daemon status: %v", err)
	}

	want := strings.Join([]string{
		"version      v-daemon",
		"pid          4123",
		"started_at   2026-09-16T08:00:00Z",
		"socket       " + filepath.Join(app.Root, api.SocketFile),
		"provider     fake",
		"pause        true",
		"resume       true",
		"fork         false",
		"plain_port   30080",
		"tls_port     30443",
	}, "\n")
	if got := strings.TrimSpace(out.String()); got != want {
		t.Errorf("daemon status printed\n%s\nwant\n%s", got, want)
	}
}

// The table lists all eight verbs in the order of the spec, the optional ones as the provider claims them.
func TestCapabilitiesPrintsEveryVerbAndWhetherTheProviderRunsIt(t *testing.T) {
	var out bytes.Buffer

	app, f := newClientApp(t, &out, models.Sandbox{})
	f.providerSvc = &fakeLifecycleProvider{r: &recorder{}, noFork: true}

	if err := app.Run(t.Context(), []string{"capabilities"}); err != nil {
		t.Fatalf("capabilities: %v", err)
	}

	want := strings.Join([]string{
		"CAPABILITY   SUPPORTED",
		"create       true",
		"start        true",
		"stop         true",
		"remove       true",
		"pause        true",
		"resume       true",
		"fork         false",
		"snapshot     true",
	}, "\n")
	if got := strings.TrimSpace(out.String()); got != want {
		t.Errorf("capabilities printed\n%s\nwant\n%s", got, want)
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

func TestListFailsWhenTheDaemonNeverAnswers(t *testing.T) {
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
	err = app.Run(t.Context(), []string{"list"})
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("list took %s to give up, want the 100ms deadline", took)
	}
	if want := "the daemon at " + filepath.Join(root, api.SocketFile) + " gave no answer within 100ms"; err == nil || err.Error() != want {
		t.Errorf("list returned %v, want %q", err, want)
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

func TestListWithNoDaemonFailsFast(t *testing.T) {
	var out bytes.Buffer

	root := shortRoot(t)
	app := App{Version: "test", Root: root, Out: &out}

	err := app.Run(t.Context(), []string{"list"})
	if want := "cannot connect to the shard daemon at " + filepath.Join(root, api.SocketFile) + ": is it running? shard --root " + root + " daemon"; err == nil || err.Error() != want {
		t.Errorf("list with no daemon returned %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Errorf("list printed %q before it failed", out.String())
	}
}
