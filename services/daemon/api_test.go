package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/sshd"
)

func TestTheAPIClosesItsSocketWhenTheHostKeyCannotBeRead(t *testing.T) {
	root, err := os.MkdirTemp("", "shard") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})
	if err := os.WriteFile(filepath.Join(root, sshd.HostKeyFile), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write the host key: %v", err)
	}

	d := &deps{cfg: Config{Root: root, Out: io.Discard}}
	err = apiTask{deps: d, lifecycle: &lifecycle{deps: d}}.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "parse the ssh host key") {
		t.Fatalf("Run = %v, want the host key refusal", err)
	}

	conn, err := net.Dial("unix", filepath.Join(root, api.SocketFile))
	if err == nil {
		t.Fatalf("the socket still takes a call: close %v", conn.Close())
	}
}
