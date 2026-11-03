package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func decodeJSON(t *testing.T, out []byte, v any) {
	t.Helper()

	if err := json.Unmarshal(out, v); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, out)
	}
}

func TestListJSONIsAnArrayOfTheRecords(t *testing.T) {
	var out bytes.Buffer

	app := newListApp(t, &out, listed(), nil)
	if err := app.Run(t.Context(), []string{"list", "--all", "--format", "json"}); err != nil {
		t.Fatalf("list --format json: %v", err)
	}

	var got []client.Sandbox
	decodeJSON(t, out.Bytes(), &got)
	if len(got) != 2 || got[0].ID != "up-1" || got[1].ID != "down-2" {
		t.Errorf("got %+v, want both records", got)
	}
}

func TestListJSONOfNoSandboxIsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer

	if err := newListApp(t, &out, nil, nil).Run(t.Context(), []string{"list", "--format", "json"}); err != nil {
		t.Fatalf("list --format json: %v", err)
	}

	if out.String() != "[]\n" {
		t.Errorf("got %q, want an empty array", out.String())
	}
}

// A record the daemon could not read still fails the list, after the whole value of the ones it could.
func TestListJSONWritesTheWholeValueBeforeItFailsOnAnUnreadableRecord(t *testing.T) {
	var out bytes.Buffer

	unreadable := &sandboxstate.UnreadableError{ID: "bad-3", Err: errors.New("decode sandbox.json of bad-3: unexpected end of JSON input")}
	err := newListApp(t, &out, listed(), unreadable).Run(t.Context(), []string{"list", "--format", "json"})
	if err == nil || !strings.Contains(err.Error(), "bad-3") {
		t.Errorf("list returned %v, want the unreadable record named", err)
	}

	var got []client.Sandbox
	decodeJSON(t, out.Bytes(), &got)
	if len(got) != 1 || got[0].ID != "up-1" {
		t.Errorf("got %+v, want the one sandbox that is up", got)
	}
}

func TestInspectTableReadsTheRecordDownAPage(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, stopped())
	if err := app.Run(t.Context(), []string{"inspect", "--format", "table", "web"}); err != nil {
		t.Fatalf("inspect --format table: %v", err)
	}

	lines := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(lines[0], "FIELD") || !strings.Contains(lines[0], "VALUE") {
		t.Errorf("the header is %q", lines[0])
	}
	if !strings.Contains(out.String(), "state ") || !strings.Contains(out.String(), "stopped\n") {
		t.Errorf("the table has no state row:\n%s", out.String())
	}
}

func TestSnapshotListJSONOfNoSnapshotIsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, stopped())
	if err := app.Run(t.Context(), []string{"snapshot", "list", "--format", "json"}); err != nil {
		t.Fatalf("snapshot list --format json: %v", err)
	}

	if out.String() != "[]\n" {
		t.Errorf("got %q, want an empty array", out.String())
	}
}

func TestSnapshotListJSONAndInspectTable(t *testing.T) {
	var out bytes.Buffer

	source := stopped()
	source.Image = "index.docker.io/library/alpine:3.20"
	source.Digest = "sha256:alpine"
	app, d := newClientApp(t, &out, source)
	d.imageSvc = fakeImages{r: &recorder{}}
	if err := app.Run(t.Context(), []string{"snapshot", "create", "--name", "web-base", "web"}); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	id := strings.TrimSpace(out.String())

	out.Reset()
	if err := app.Run(t.Context(), []string{"snapshot", "list", "--format", "json"}); err != nil {
		t.Fatalf("snapshot list --format json: %v", err)
	}
	var got []models.Snapshot
	decodeJSON(t, out.Bytes(), &got)
	if len(got) != 1 || got[0].ID != id || got[0].Name != "web-base" || got[0].Source != source.ID {
		t.Errorf("got %+v, want the one snapshot of %s", got, source.ID)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"snapshot", "inspect", "--format", "table", "web-base"}); err != nil {
		t.Fatalf("snapshot inspect --format table: %v", err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(lines[0], "FIELD") || !strings.HasPrefix(lines[1], "id ") || !strings.HasSuffix(lines[1], id) {
		t.Errorf("snapshot inspect printed:\n%s", out.String())
	}
}

func TestImageListJSONOfAnEmptyStoreIsAnEmptyArray(t *testing.T) {
	var out bytes.Buffer

	app, _ := newStoreApp(t, &out)
	if err := app.Run(t.Context(), []string{"image", "list", "--format", "json"}); err != nil {
		t.Fatalf("image list --format json: %v", err)
	}

	if out.String() != "[]\n" {
		t.Errorf("got %q, want an empty array", out.String())
	}
}

// The JSON names the secret, where it goes and its placeholder, and never the value.
func TestSecretListJSONNeverHoldsTheValue(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "sk-live-abcdef123456\n", &fakeLifecycleRepo{r: &recorder{}})
	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "API_TOKEN"}); err != nil {
		t.Fatalf("secret set: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "list", "--format", "json"}); err != nil {
		t.Fatalf("secret list --format json: %v", err)
	}

	if strings.Contains(out.String(), "sk-live") {
		t.Fatalf("the JSON holds the value:\n%s", out.String())
	}
	var got []map[string]any
	decodeJSON(t, out.Bytes(), &got)
	if len(got) != 1 || got[0]["name"] != "API_TOKEN" || got[0]["placeholder"] != "mock-API_TOKEN" {
		t.Errorf("got %v, want API_TOKEN with its placeholder", got)
	}
}

func TestPolicyListJSONAndShowTable(t *testing.T) {
	var out bytes.Buffer

	app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())
	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "api.example.com", "--deny", "any", "web"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "list", "--format", "json"}); err != nil {
		t.Fatalf("policy list --format json: %v", err)
	}
	var summaries []policySummary
	decodeJSON(t, out.Bytes(), &summaries)
	if len(summaries) != 1 || summaries[0].Name != "web" || summaries[0].RuleCount != 2 {
		t.Errorf("got %+v, want web with 2 rules", summaries)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "show", "--format", "table", "web"}); err != nil {
		t.Fatalf("policy show --format table: %v", err)
	}
	if !strings.HasPrefix(out.String(), "FIELD") || !strings.Contains(out.String(), "\nRULE\nallow api.example.com tcp:80,443\ndeny any\n") {
		t.Errorf("policy show printed:\n%s", out.String())
	}
}

func TestTokensListJSONAndMintTable(t *testing.T) {
	var out bytes.Buffer

	dir := t.TempDir()
	key := filepath.Join(dir, "secret")
	if err := os.WriteFile(key, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the signing key: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: &out}

	if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--format", "table", "--signing-key-file", key}); err != nil {
		t.Fatalf("tokens mint --format table: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "TOKEN") || !strings.Contains(lines[1], "never") {
		t.Errorf("mint printed:\n%s", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"tokens", "list", "--format", "json", "--signing-key-file", key}); err != nil {
		t.Fatalf("tokens list --format json: %v", err)
	}
	var got []tokenView
	decodeJSON(t, out.Bytes(), &got)
	if len(got) != 1 || got[0].Name != "ci" || got[0].ExpiresAt != nil || got[0].Status != "active" {
		t.Errorf("got %+v, want the ci token, active, with no expiry", got)
	}
}

func TestInfoJSONNamesTheProviderAndWhy(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"info", "--format", "json"}); err != nil {
		t.Fatalf("info --format json: %v", err)
	}

	var got infoView
	decodeJSON(t, out.Bytes(), &got)
	if got.Provider == "" || got.Reason == "" {
		t.Errorf("got %+v, want a provider and a reason", got)
	}
}

// A task in backoff still fails the status, after the whole JSON value.
func TestDaemonStatusJSONWritesTheValueThenFailsOnABackoff(t *testing.T) {
	root := shortRoot(t)
	serveFakeDaemon(t, root, api.Daemon{
		Version:  "v-test",
		PID:      7,
		Provider: "gvisor",
		Tasks:    []api.TaskState{{Name: "liveness", State: "backoff", Restarts: 3, LastError: "runsc is gone"}},
	})
	out := &syncBuffer{}

	err := App{Version: "v-test", Root: root, Out: out}.Run(t.Context(), []string{"daemon", "status", "--format", "json"})
	if err == nil || !strings.Contains(err.Error(), "liveness") {
		t.Errorf("status returned %v, want an error naming liveness", err)
	}

	var got api.Daemon
	decodeJSON(t, []byte(out.String()), &got)
	if got.PID != 7 || len(got.Tasks) != 1 || got.Tasks[0].LastError != "runsc is gone" {
		t.Errorf("got %+v, want the whole status", got)
	}
}

// The CLI writes the object the route answers, so a script reads the same keys either way.
func TestCapabilitiesJSONIsTheObjectTheRouteAnswers(t *testing.T) {
	var out bytes.Buffer

	app, f := newClientApp(t, &out, models.Sandbox{})
	f.providerSvc = &fakeLifecycleProvider{r: &recorder{}, noPause: true, noResume: true}
	if err := app.Run(t.Context(), []string{"capabilities", "--format", "json"}); err != nil {
		t.Fatalf("capabilities --format json: %v", err)
	}

	want := `{"create":true,"start":true,"stop":true,"remove":true,"pause":false,"resume":false,"fork":true,"snapshot":true,"port":true,"swap":false}`
	var got bytes.Buffer
	if err := json.Compact(&got, out.Bytes()); err != nil || got.String() != want {
		t.Errorf("capabilities --format json wrote %s (%v), want %s", out.String(), err, want)
	}
}

func TestVersionJSONNamesBothVersions(t *testing.T) {
	var out bytes.Buffer

	app, _ := newStoreApp(t, &out)
	if err := app.Run(t.Context(), []string{"version", "--format", "json"}); err != nil {
		t.Fatalf("version --format json: %v", err)
	}

	var got versionView
	decodeJSON(t, out.Bytes(), &got)
	if got.Client != "test" || got.Daemon == "" || got.Shim != shimState() {
		t.Errorf("got %+v, want the client, the daemon and the shim state", got)
	}
}

// With no daemon there is no whole value, so the client version alone is not printed.
func TestVersionJSONWithNoDaemonWritesNothing(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"version", "--format", "json"}); err == nil {
		t.Fatal("version --format json with no daemon succeeded")
	}

	if out.Len() != 0 {
		t.Errorf("stdout holds %q, want nothing", out.String())
	}
}
