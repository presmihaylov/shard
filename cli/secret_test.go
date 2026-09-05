package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/secret"
)

// newSecretApp drives the real store under a temporary root, with stdin replaced by what the test pipes in.
func newSecretApp(t *testing.T, out *bytes.Buffer, stdin string, repo sandboxRepo) (App, string) {
	t.Helper()

	root := t.TempDir()

	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })

	app := App{Version: "test", Root: root, Out: out, Err: out, Timeout: time.Minute}
	app.newDeps = func(a App) *deps { return &deps{app: a, inFile: in, repoSvc: repo} }

	return app, root
}

func TestSecretSetLsRmRoundTrip(t *testing.T) {
	var out bytes.Buffer

	app, root := newSecretApp(t, &out, "sk-live-abcdef123456\n", &fakeLifecycleRepo{r: &recorder{}})

	if err := app.Run(t.Context(), []string{"secret", "set", "--to", "api.openai.com", "OPENAI_API_KEY"}); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "OPENAI_API_KEY" {
		t.Errorf("set printed %q, want the name", got)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "ls"}); err != nil {
		t.Fatalf("secret ls: %v", err)
	}
	if !strings.Contains(out.String(), "OPENAI_API_KEY") || !strings.Contains(out.String(), "api.openai.com") || !strings.Contains(out.String(), "mock-OPENAI_API_KEY") {
		t.Errorf("ls printed:\n%s", out.String())
	}
	if strings.Contains(out.String(), "sk-live") {
		t.Fatalf("ls printed the value:\n%s", out.String())
	}

	// The value lives in exactly one file, and a grep of everything else under the root finds nothing.
	found := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(blob, []byte("sk-live-abcdef123456")) {
			found++
			if path != filepath.Join(root, "secrets", "OPENAI_API_KEY") {
				t.Errorf("the value is in %s", path)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Errorf("the value is in %d files, want the one store file", found)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "rm", "OPENAI_API_KEY"}); err != nil {
		t.Fatalf("secret rm: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "ls"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "OPENAI_API_KEY") {
		t.Errorf("ls still lists the removed secret:\n%s", out.String())
	}
}

func TestSecretSetRefusesAnEmptyStdinAndNoDestination(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "\n", nil)

	err := app.Run(t.Context(), []string{"secret", "set", "--to", "api.example.com", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("set with an empty stdin = %v", err)
	}

	app, _ = newSecretApp(t, &out, "value-123456\n", nil)

	err = app.Run(t.Context(), []string{"secret", "set", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "no destination") {
		t.Errorf("set with no --to = %v", err)
	}

	err = app.Run(t.Context(), []string{"secret", "set", "KEY", "--to", "api.example.com"})
	if err == nil || !strings.Contains(err.Error(), "before the name") {
		t.Errorf("set with the flag after the name = %v", err)
	}
}

func TestSecretLsListsTheReadableOnesAndFails(t *testing.T) {
	var out bytes.Buffer

	app, root := newSecretApp(t, &out, "value-123456\n", &fakeLifecycleRepo{r: &recorder{}})

	if err := app.Run(t.Context(), []string{"secret", "set", "--to", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "BROKEN"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := app.Run(t.Context(), []string{"secret", "ls"})
	if err == nil || !strings.Contains(err.Error(), "BROKEN") {
		t.Errorf("ls with a broken file = %v", err)
	}
	if !strings.Contains(out.String(), "KEY") {
		t.Errorf("ls did not list the readable secret:\n%s", out.String())
	}

	if err := app.Run(t.Context(), []string{"secret", "rm", "BROKEN"}); err != nil {
		t.Errorf("rm of the broken file = %v", err)
	}
}

func TestSecretRmRefusesWhileASandboxHoldsIt(t *testing.T) {
	var out bytes.Buffer

	repo := &fakeLifecycleRepo{r: &recorder{}, left: []models.Sandbox{
		{ID: "sandbox1", State: models.StateStopped, Secrets: []string{"KEY"}},
		{ID: "sandbox2", State: models.StateRunning, Secrets: []string{"OTHER"}},
	}}
	app, root := newSecretApp(t, &out, "value-123456\n", repo)

	if err := app.Run(t.Context(), []string{"secret", "set", "--to", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}

	err := app.Run(t.Context(), []string{"secret", "rm", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || strings.Contains(err.Error(), "sandbox2") {
		t.Errorf("rm = %v, want a refusal naming sandbox1 only", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "KEY")); err != nil {
		t.Errorf("a refused rm removed the secret: %v", err)
	}

	if err := app.Run(t.Context(), []string{"secret", "rm", "--force", "KEY"}); err != nil {
		t.Errorf("rm --force = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "KEY")); err == nil {
		t.Error("rm --force left the secret")
	}
}

func TestSecretRmRefusesWhenARecordIsUnreadable(t *testing.T) {
	var out bytes.Buffer

	repo := &fakeLifecycleRepo{r: &recorder{}, unreadable: os.ErrPermission}
	app, _ := newSecretApp(t, &out, "value-123456\n", repo)

	if err := app.Run(t.Context(), []string{"secret", "set", "--to", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}

	err := app.Run(t.Context(), []string{"secret", "rm", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("rm with an unreadable record = %v", err)
	}
}

func TestSecretRmOfAMissingSecretFails(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "", &fakeLifecycleRepo{r: &recorder{}})

	err := app.Run(t.Context(), []string{"secret", "rm", "NOPE"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("rm of a missing secret = %v", err)
	}
}

func TestParseSecretSetTakesHeadersAndAMatch(t *testing.T) {
	opts, err := parseSecretSet([]string{
		"--to", "api.example.com",
		"--header", "Authorization: Bearer {value}",
		"--match", "path=/v1/*",
		"--match", "method=get,post",
		"--match", "query=k=v",
		"--match", "header=X-Want: yes",
		"TOKEN",
	})
	if err != nil {
		t.Fatalf("parseSecretSet: %v", err)
	}
	if len(opts.headers) != 1 || opts.headers[0].Name != "Authorization" || opts.headers[0].Value != "Bearer {value}" {
		t.Errorf("the headers read %+v", opts.headers)
	}
	m := opts.match
	if m == nil || m.Path != "/v1/*" || len(m.Methods) != 2 || m.Methods[0] != "GET" {
		t.Fatalf("the match reads %+v", m)
	}
	if len(m.Query) != 1 || m.Query[0] != (secret.Pair{Name: "k", Value: "v"}) {
		t.Errorf("the query match reads %+v", m.Query)
	}
	if len(m.Headers) != 1 || m.Headers[0] != (secret.Pair{Name: "X-Want", Value: "yes"}) {
		t.Errorf("the header match reads %+v", m.Headers)
	}
}

func TestParseSecretSetRefusesABadHeaderOrMatch(t *testing.T) {
	for name, args := range map[string][]string{
		"a header with no colon":    {"--header", "Authorization"},
		"a header with no name":     {"--header", ": x"},
		"a second path match":       {"--match", "path=/a", "--match", "path=/b"},
		"an unknown dimension":      {"--match", "host=x"},
		"a query with no value":     {"--match", "query=k"},
		"a match with no condition": {"--match", "path="},
	} {
		if _, err := parseSecretSet(append(args, "TOKEN")); err == nil {
			t.Errorf("parseSecretSet took %s", name)
		}
	}
}

// newGrantApp is newSecretApp with a proxy fake wired in, since a grant fetches the proxy CA.
func newGrantApp(t *testing.T, out *bytes.Buffer, stdin string, repo sandboxRepo, r *recorder) App {
	t.Helper()

	app, _ := newSecretApp(t, out, stdin, repo)
	base := app.newDeps
	app.newDeps = func(a App) *deps {
		d := base(a)
		d.proxySvc = fakeProxy{r: r}

		return d
	}

	return app
}

// newGrantStateDir lays a bundle on disk the way create leaves one for an unfronted sandbox.
func newGrantStateDir(t *testing.T, env []string) string {
	t.Helper()

	stateDir := t.TempDir()
	rootFS := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A grant fronts the sandbox, and that refuses an image that ships no roots.
	if err := os.MkdirAll(filepath.Join(rootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootFS, "etc/ssl/certs/ca-certificates.crt"), []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := `{"process":{"args":["/init"],"cwd":"/","env":["` + strings.Join(env, `","`) + `"]},` +
		`"annotations":{"dev.shard.rootfs":"` + rootFS + `"}}`
	if err := os.WriteFile(filepath.Join(stateDir, "bundle", "config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	return stateDir
}

func grantConfig(t *testing.T, stateDir string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(stateDir, "bundle", "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

func TestSecretGrantAndUngrantRoundTrip(t *testing.T) {
	var out bytes.Buffer
	ctx := context.Background()

	r := &recorder{}
	stateDir := newGrantStateDir(t, []string{"PATH=/usr/bin"})
	repo := &fakeLifecycleRepo{r: r, sb: models.Sandbox{ID: "sandbox1", State: models.StateStopped}, dir: stateDir}
	app := newGrantApp(t, &out, "value-123456\n", repo, r)

	if err := app.Run(ctx, []string{"secret", "set", "--to", "api.example.com", "API_KEY"}); err != nil {
		t.Fatalf("secret set: %v", err)
	}

	if err := app.Run(ctx, []string{"secret", "grant", "sandbox1", "API_KEY"}); err != nil {
		t.Fatalf("secret grant: %v", err)
	}
	if !strings.Contains(out.String(), "sandbox1") {
		t.Errorf("output %q lacks the id", out.String())
	}
	if len(repo.sb.Secrets) != 1 || repo.sb.Secrets[0] != "API_KEY" {
		t.Errorf("record secrets = %v", repo.sb.Secrets)
	}
	config := grantConfig(t, stateDir)
	if !strings.Contains(config, "API_KEY=mock-API_KEY") {
		t.Errorf("config %s lacks the placeholder", config)
	}
	if !strings.Contains(config, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("config %s lacks the proxy CA environment", config)
	}

	err := app.Run(ctx, []string{"secret", "grant", "sandbox1", "API_KEY"})
	if err == nil || !strings.Contains(err.Error(), "already holds secret API_KEY") {
		t.Errorf("a second grant = %v", err)
	}

	if err := app.Run(ctx, []string{"secret", "ungrant", "sandbox1", "API_KEY"}); err != nil {
		t.Fatalf("secret ungrant: %v", err)
	}
	if len(repo.sb.Secrets) != 0 {
		t.Errorf("record secrets after the ungrant = %v", repo.sb.Secrets)
	}
	if strings.Contains(grantConfig(t, stateDir), "API_KEY=") {
		t.Error("the placeholder outlived the ungrant")
	}

	err = app.Run(ctx, []string{"secret", "ungrant", "sandbox1", "API_KEY"})
	if err == nil || !strings.Contains(err.Error(), "does not hold secret API_KEY") {
		t.Errorf("a second ungrant = %v", err)
	}
}

func TestSecretGrantRefusesALiveSandbox(t *testing.T) {
	for _, state := range []models.State{models.StateRunning, models.StatePaused} {
		var out bytes.Buffer

		r := &recorder{}
		repo := &fakeLifecycleRepo{r: r, sb: models.Sandbox{ID: "sandbox1", State: state}}
		app := newGrantApp(t, &out, "value-123456\n", repo, r)

		err := app.Run(context.Background(), []string{"secret", "grant", "sandbox1", "API_KEY"})
		if err == nil || !strings.Contains(err.Error(), "stop it first") {
			t.Errorf("grant on a %s sandbox = %v", state, err)
		}
	}
}

func TestSecretGrantRefusesWhatWouldCollideOrIsMissing(t *testing.T) {
	var out bytes.Buffer
	ctx := context.Background()

	r := &recorder{}
	stateDir := newGrantStateDir(t, []string{"API_KEY=user-set"})
	repo := &fakeLifecycleRepo{r: r, sb: models.Sandbox{ID: "sandbox1", State: models.StateCreated}, dir: stateDir}
	app := newGrantApp(t, &out, "value-123456\n", repo, r)

	err := app.Run(ctx, []string{"secret", "grant", "sandbox1", "NO_SUCH"})
	if err == nil || !strings.Contains(err.Error(), "NO_SUCH") {
		t.Errorf("grant of a missing secret = %v", err)
	}

	if err := app.Run(ctx, []string{"secret", "set", "--to", "api.example.com", "API_KEY"}); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	err = app.Run(ctx, []string{"secret", "grant", "sandbox1", "API_KEY"})
	if err == nil || !strings.Contains(err.Error(), "already holds API_KEY") {
		t.Errorf("grant over a held name = %v", err)
	}
	if len(repo.sb.Secrets) != 0 {
		t.Errorf("record secrets after a refused grant = %v", repo.sb.Secrets)
	}
}
