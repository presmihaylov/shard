//go:build integration

package cli

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
)

// The daemon and the CLI share the root, so what one wrote the other serves.
func TestDaemonListsTheSandboxTheCLICreated(t *testing.T) {
	app, out := newCreateApp(t)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	path := filepath.Join(app.Root, api.SocketFile)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if info.Mode().Type() != fs.ModeSocket || info.Mode().Perm() != 0o600 {
		t.Errorf("the socket is %s, want a socket at 0600 on a host with no shard group", info.Mode())
	}

	client := socketClient(app.Root)

	resp, err := client.Get("http://shard/v0/sandboxes")
	if err != nil {
		t.Fatalf("GET /v0/sandboxes: %v", err)
	}
	defer resp.Body.Close()

	var list struct {
		Sandboxes []models.Sandbox `json:"sandboxes"`
		Warnings  []string         `json:"warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode the list: %v", err)
	}
	if resp.StatusCode != http.StatusOK || len(list.Warnings) != 0 {
		t.Fatalf("GET /v0/sandboxes answered %d with the warnings %v", resp.StatusCode, list.Warnings)
	}
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != id || list.Sandboxes[0].State != models.StateRunning {
		t.Fatalf("the daemon lists %+v, want the running sandbox %s", list.Sandboxes, id)
	}

	one, err := client.Get("http://shard/v0/sandboxes/" + id)
	if err != nil {
		t.Fatalf("GET /v0/sandboxes/%s: %v", id, err)
	}
	defer one.Body.Close()

	var sb models.Sandbox
	if err := json.NewDecoder(one.Body).Decode(&sb); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	if one.StatusCode != http.StatusOK || sb.ID != id || sb.PID == 0 {
		t.Errorf("GET /v0/sandboxes/%s answered %d %+v, want the live record", id, one.StatusCode, sb)
	}
}
