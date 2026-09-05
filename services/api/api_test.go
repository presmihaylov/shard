package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// seed wires the service over a repository on disk under a temporary root, with one running sandbox
// named web that holds the policy locked, and one stopped sandbox.
func seed(t *testing.T) (*Service, models.Sandbox, models.Sandbox) {
	t.Helper()

	svc, up, down := seedUnder(t, t.TempDir())

	return svc, up, down
}

func seedUnder(t *testing.T, root string) (*Service, models.Sandbox, models.Sandbox) {
	t.Helper()

	repo, err := sandboxstate.New(root)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}

	policies, err := egress.NewStore(filepath.Join(root, "policies"))
	if err != nil {
		t.Fatalf("policies: %v", err)
	}
	locked := models.Policy{Name: "locked", Rules: []models.Rule{
		{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationCIDR, Value: "10.0.0.0/8"}},
	}}
	if err := policies.Set(locked); err != nil {
		t.Fatalf("store the policy: %v", err)
	}

	secrets, err := secret.New(filepath.Join(root, "secrets"))
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}

	up, err := repo.Create(models.Sandbox{Name: "web", Image: "alpine:3.20", Provider: "gvisor", State: models.StateRunning, Address: netip.MustParsePrefix("10.44.0.2/24"), Policy: "locked", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("create the running sandbox: %v", err)
	}
	down, err := repo.Create(models.Sandbox{Image: "alpine:3.20", Provider: "gvisor", State: models.StateStopped, CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("create the stopped sandbox: %v", err)
	}

	svc := &Service{Binary: "v0.test", Records: repo, Egress: egress.New(policies, repo, secrets, nil, nil)}

	return svc, up, down
}

func get(t *testing.T, h http.Handler, path string, into any) int {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s answered Content-Type %q, want application/json", path, ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("GET %s answered %q, which does not decode: %v", path, rec.Body.String(), err)
	}

	return rec.Code
}

func TestVersion(t *testing.T) {
	svc, _, _ := seed(t)

	var got VersionInfo
	if code := get(t, svc.Handler(), "/v0/version", &got); code != http.StatusOK {
		t.Fatalf("version answered %d", code)
	}
	if got.Version != "v0.test" || got.APIVersion != Version {
		t.Errorf("version answered %+v", got)
	}
}

func TestListShowsWhatIsUp(t *testing.T) {
	svc, up, down := seed(t)

	var got []models.Sandbox
	if code := get(t, svc.Handler(), "/v0/sandboxes", &got); code != http.StatusOK {
		t.Fatalf("list answered %d", code)
	}
	if len(got) != 1 || got[0].ID != up.ID || got[0].Name != "web" {
		t.Errorf("list answered %+v, want only %s", got, up.ID)
	}

	if code := get(t, svc.Handler(), "/v0/sandboxes?all=true", &got); code != http.StatusOK {
		t.Fatalf("list --all answered %d", code)
	}
	if len(got) != 2 {
		t.Errorf("list with all=true answered %d sandboxes, want %s and %s", len(got), up.ID, down.ID)
	}
}

func TestListRefusesAnUnreadableRecord(t *testing.T) {
	root := t.TempDir()
	svc, up, _ := seedUnder(t, root)

	if err := os.WriteFile(filepath.Join(root, "sandboxes", up.ID, "sandbox.json"), []byte("{"), 0o640); err != nil {
		t.Fatalf("break the record: %v", err)
	}

	var got Error
	if code := get(t, svc.Handler(), "/v0/sandboxes", &got); code != http.StatusInternalServerError {
		t.Fatalf("list over a broken record answered %d", code)
	}
	if !strings.Contains(got.Error, up.ID) {
		t.Errorf("the error %q does not name the broken sandbox %s", got.Error, up.ID)
	}
}

func TestGetByIDAndByName(t *testing.T) {
	svc, up, _ := seed(t)

	for _, ref := range []string{up.ID, "web"} {
		var got Sandbox
		if code := get(t, svc.Handler(), "/v0/sandboxes/"+ref, &got); code != http.StatusOK {
			t.Fatalf("get %s answered %d", ref, code)
		}
		if got.ID != up.ID || got.State != models.StateRunning {
			t.Errorf("get %s answered %+v", ref, got.Sandbox)
		}
		if got.Egress == nil || got.Egress.Policy != "locked" || len(got.Egress.Rules) == 0 {
			t.Errorf("get %s answered egress %+v, want the locked rules", ref, got.Egress)
		}
	}
}

func TestGetWithoutAPolicyHasNoEgress(t *testing.T) {
	svc, _, down := seed(t)

	var got map[string]json.RawMessage
	if code := get(t, svc.Handler(), "/v0/sandboxes/"+down.ID, &got); code != http.StatusOK {
		t.Fatalf("get answered %d", code)
	}
	if _, ok := got["egress"]; ok {
		t.Errorf("a sandbox with no policy answered an egress field: %s", got["egress"])
	}
}

func TestGetUnknownIs404(t *testing.T) {
	svc, _, _ := seed(t)

	for _, ref := range []string{"nothing-here-0000", "no-such-name", "bad%20ref"} {
		var got Error
		if code := get(t, svc.Handler(), "/v0/sandboxes/"+ref, &got); code != http.StatusNotFound {
			t.Errorf("get %s answered %d, want 404", ref, code)
		}
		if got.Error == "" {
			t.Errorf("get %s answered no error body", ref)
		}
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	svc, _, _ := seed(t)

	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v0/sandboxes/web", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE answered %d, want 405", rec.Code)
	}
}

// The socket carries the same handler; the group branch needs a host with the group, so this
// covers the root-only fallback and the stale-file removal.
func TestListenServesOverTheSocket(t *testing.T) {
	svc, up, _ := seed(t)

	path := filepath.Join(t.TempDir(), SocketFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("plant a stale socket: %v", err)
	}

	l, mode, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if !strings.HasPrefix(mode, "mode 0660") && !strings.HasPrefix(mode, "mode 0600") {
		t.Errorf("Listen reported %q", mode)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o660 && perm != 0o600 {
		t.Errorf("the socket is mode %04o", perm)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, svc.Handler()) }()

	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get("http://shard/v0/sandboxes/" + up.ID)
	if err != nil {
		t.Fatalf("GET over the socket: %v", err)
	}
	defer resp.Body.Close()

	var got Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusOK || got.ID != up.ID {
		t.Errorf("the socket answered %d %+v", resp.StatusCode, got.Sandbox)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve ended with %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the socket file outlived the daemon")
	}
}
