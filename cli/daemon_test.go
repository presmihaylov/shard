package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/api"
)

func TestDaemonTakesNoArgumentButStatus(t *testing.T) {
	err := App{Out: io.Discard}.Run(t.Context(), []string{"daemon", "extra"})
	if want := `daemon takes no arguments, or status, got ["extra"]`; err == nil || err.Error() != want {
		t.Errorf("daemon with an argument got %v, want %q", err, want)
	}
}

// A name no provider answers to fails before the daemon takes the root, so nothing is left under it.
func TestDaemonRefusesAProviderShardDoesNotKnow(t *testing.T) {
	root := shortRoot(t)

	err := App{Version: "v-test", Root: root, Out: io.Discard}.Run(t.Context(), []string{"daemon", "--provider", "vmware"})
	if want := "unknown provider \"vmware\": shard knows gvisor, sysbox, runc, vz and firecracker"; err == nil || err.Error() != want {
		t.Errorf("daemon --provider vmware returned %v, want %q", err, want)
	}
	if _, err := os.Lstat(filepath.Join(root, api.SocketFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused daemon left a socket: %v", err)
	}
}

// syncBuffer is a bytes.Buffer the daemon writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// itestPrefix names the temp roots the integration teardown sweeps, so a root a test here leaves behind goes with them.
const itestPrefix = "shard-itest"

// shortTemp is where shortRoot makes a root: t.TempDir and a Mac's $TMPDIR are both past the root a vz sandbox's socket path leaves room for.
const shortTemp = "/tmp"

// A root this helper made and a test failed to remove is still the sweep's to take back (SHARD-377).
func TestShortRootSitsUnderThePrefixTheSweepOwns(t *testing.T) {
	root := shortRoot(t)
	if filepath.Dir(root) != shortTemp || !strings.HasPrefix(filepath.Base(root), itestPrefix) {
		t.Errorf("shortRoot = %s, want %s* under %s", root, itestPrefix, shortTemp)
	}
}

// shortRoot makes a root short enough for a socket path, under the prefix the integration sweep owns.
func shortRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp(shortTemp, itestPrefix) //nolint:usetesting // t.TempDir is too long for a socket path
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

// startDaemon waits for the log line, not the file: the socket exists a moment before its mode is set.
func startDaemon(t *testing.T, app App, out *syncBuffer, flags ...string) (context.CancelFunc, <-chan error) {
	t.Helper()

	app.Out = out
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, append([]string{"--root", app.Root, "daemon"}, flags...)) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "api listening on") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the daemon never logged the socket:\n%s", out.String())
		}
		time.Sleep(time.Millisecond)
	}

	return cancel, done
}

func socketClient(root string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(root, api.SocketFile))
	}}}
}

// serveFakeDaemon answers GET /v0/daemon on the socket under root with a fixed record, so a status test needs no provider.
func serveFakeDaemon(t *testing.T, root string, d api.Daemon) {
	t.Helper()

	ln, err := net.Listen("unix", filepath.Join(root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen on the socket: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/daemon", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(d); err != nil {
			t.Errorf("encode the daemon record: %v", err)
		}
	})
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve the fake daemon: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close the fake daemon: %v", err)
		}
	})
}

func TestDaemonStatusExitsNonZeroWhenATaskIsInBackoff(t *testing.T) {
	root := shortRoot(t)
	serveFakeDaemon(t, root, api.Daemon{
		Version:  "v-test",
		PID:      7,
		Provider: "gvisor",
		Tasks: []api.TaskState{
			{Name: "dns", State: "running"},
			{Name: "liveness", State: "backoff", Restarts: 3, LastError: "runsc is gone"},
		},
	})
	out := &syncBuffer{}

	err := App{Version: "v-test", Root: root, Out: out}.Run(t.Context(), []string{"daemon", "status"})
	if err == nil || !strings.Contains(err.Error(), "backoff") || !strings.Contains(err.Error(), "liveness") {
		t.Errorf("status with a task in backoff returned %v, want an error naming liveness", err)
	}
	if s := out.String(); !strings.Contains(s, "liveness") || !strings.Contains(s, "backoff") || !strings.Contains(s, "runsc is gone") {
		t.Errorf("status output = %q, want the task table with the backoff task", s)
	}
}

func TestDaemonStatusListsTheTasksAndSucceedsWhenAllRun(t *testing.T) {
	root := shortRoot(t)
	serveFakeDaemon(t, root, api.Daemon{
		Version:  "v-test",
		PID:      7,
		Provider: "gvisor",
		Tasks: []api.TaskState{
			{Name: "api", State: "running"},
			{Name: "dns", State: "running"},
		},
	})
	out := &syncBuffer{}

	if err := (App{Version: "v-test", Root: root, Out: out}).Run(t.Context(), []string{"daemon", "status"}); err != nil {
		t.Fatalf("status with every task running returned %v, want nil", err)
	}
	for _, want := range []string{"task", "state", "restarts", "api", "dns", "running"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output = %q, want it to contain %q", out.String(), want)
		}
	}
}

// The daemon needs no runsc for this: the socket and the records are plain files, and gvisor keeps a /dev/kvm host from provisioning a data image.
func TestDaemonAnswersOnTheSocketUntilTheContextEnds(t *testing.T) {
	root := shortRoot(t)
	out := &syncBuffer{}

	cancel, done := startDaemon(t, App{Version: "v-test", Root: root}, out, "--provider", "gvisor")

	resp, err := socketClient(root).Get("http://shard/v0/version")
	if err != nil {
		t.Fatalf("GET /v0/version over the socket: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode the version: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body.Version != "v-test" {
		t.Errorf("GET /v0/version answered %d %q, want 200 and v-test", resp.StatusCode, body.Version)
	}

	if log := out.String(); !strings.Contains(log, "api listening on "+filepath.Join(root, api.SocketFile)+", mode 0") {
		t.Errorf("the daemon did not log the socket and its mode:\n%s", log)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon ended with %v", err)
	}

	if _, err := os.Lstat(filepath.Join(root, api.SocketFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket outlived the daemon: %v", err)
	}
}
