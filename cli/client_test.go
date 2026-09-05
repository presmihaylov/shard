package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	for _, arg := range []string{"version", "--version"} {
		var out bytes.Buffer

		app, _ := newClientApp(t, &out, models.Sandbox{})

		if err := app.Run(t.Context(), []string{arg}); err != nil {
			t.Fatalf("Run(%q): %v", arg, err)
		}

		if got := strings.TrimSpace(out.String()); got != "client test\ndaemon v-daemon" {
			t.Errorf("Run(%q) printed %q, want the client line and the daemon line", arg, got)
		}
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
