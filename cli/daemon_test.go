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

func TestDaemonTakesNoArguments(t *testing.T) {
	err := App{Out: io.Discard}.Run(t.Context(), []string{"daemon", "extra"})
	if err == nil || !strings.Contains(err.Error(), "daemon takes no arguments") {
		t.Errorf("daemon with an argument got %v", err)
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

// startDaemon waits for the log line, not the file: the socket exists a moment before its mode is set.
func startDaemon(t *testing.T, app App, out *syncBuffer) (context.CancelFunc, <-chan error) {
	t.Helper()

	app.Out = out
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, []string{"--root", app.Root, "daemon"}) }()

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

// The daemon needs no provider and no runsc for this: the socket and the records are plain files.
func TestDaemonAnswersOnTheSocketUntilTheContextEnds(t *testing.T) {
	root := shortRoot(t)
	out := &syncBuffer{}

	cancel, done := startDaemon(t, App{Version: "v-test", Root: root}, out)

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
