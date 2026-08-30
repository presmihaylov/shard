package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
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

func TestSecretSetRefusesANewPlaceholderWhileASandboxHoldsIt(t *testing.T) {
	var out bytes.Buffer

	repo := &fakeLifecycleRepo{r: &recorder{}, left: []models.Sandbox{{ID: "sb1", Secrets: []string{"KEY"}}}}
	app, root := newSecretApp(t, &out, "value-123456\n", repo)

	if err := app.Run(t.Context(), []string{"secret", "set", "--to", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}

	app, _ = newSecretApp(t, &out, "value-654321\n", repo)
	app.Root = root
	err := app.Run(t.Context(), []string{"secret", "set", "--mock-value", "another-placeholder", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "sb1") {
		t.Errorf("set with a new placeholder while held = %v", err)
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
