package runsc_test

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/runsc"
)

// shortExecDir keeps the socket path under the 104 bytes a Mac allows, which t.TempDir passes.
func shortExecDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rs") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("make the exec dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})

	return dir
}

// streamer is a fake runsc port-forward that dials the --stream socket, writes there, and exits as runsc does once the sandbox holds the stream.
const streamer = `while [ "$1" != "--stream" ]; do shift; done
python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); s.sendall(b"from the sandbox"); s.close()' "$2"
`

func TestPortForwardAnswersTheStreamRunscDialed(t *testing.T) {
	execDir := shortExecDir(t)
	r, argvFile := fakeBinary(t, streamer, runsc.WithExecDir(execDir))

	conn, err := r.PortForward(t.Context(), "amber-otter-1a2b", 8100)
	if err != nil {
		t.Fatalf("PortForward: %v", err)
	}
	defer conn.Close()
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "from the sandbox" {
		t.Fatalf("read %q, %v; want what the fake wrote", got, err)
	}

	args := argv(t, argvFile)
	at := slices.Index(args, "port-forward")
	if at < 0 || !slices.Equal(args[at+3:], []string{"amber-otter-1a2b", "8100"}) || args[at+1] != "--stream" {
		t.Fatalf("argv %q, want port-forward --stream <socket> amber-otter-1a2b 8100", args)
	}
	entries, err := os.ReadDir(execDir)
	if err != nil {
		t.Fatalf("read the exec directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("PortForward left %d entries under %s, want none", len(entries), execDir)
	}
}

func TestPortForwardKeepsWhatRunscSaidWhenItFails(t *testing.T) {
	execDir := shortExecDir(t)
	r, _ := fakeBinary(t, "echo 'dial 127.0.0.1:8100: connection refused' >&2\nexit 128\n", runsc.WithExecDir(execDir))

	_, err := r.PortForward(t.Context(), "amber-otter-1a2b", 8100)
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("PortForward gave %v, want runsc's own words", err)
	}
	entries, err := os.ReadDir(execDir)
	if err != nil {
		t.Fatalf("read the exec directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("PortForward left %d entries under %s, want none", len(entries), execDir)
	}
}
