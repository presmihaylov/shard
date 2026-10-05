package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/secret"
)

// newSecretApp puts a daemon on the real store up, with stdin replaced by what the test pipes in.
func newSecretApp(t *testing.T, out *bytes.Buffer, stdin string, repo sandboxRepo) (App, string) {
	t.Helper()

	root := shortRoot(t)

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

	holders := func(name string) ([]string, error) { return sandbox.SecretHolders(repo, name) }

	secrets, err := secret.New(filepath.Join(root, "secrets"), holders)
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	policies, err := egress.NewStore(filepath.Join(root, "policies"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	serveDaemon(t, &fakeDaemon{app: App{Root: root}, repoSvc: repo, secretSvc: secrets, policySvc: policies})

	return App{Version: "test", Root: root, Out: out, Err: out, in: in}, root
}

func TestSecretSetListRemoveRoundTrip(t *testing.T) {
	var out bytes.Buffer

	app, root := newSecretApp(t, &out, "sk-live-abcdef123456\n", &fakeLifecycleRepo{r: &recorder{}})

	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.openai.com", "OPENAI_API_KEY"}); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "OPENAI_API_KEY" {
		t.Errorf("set printed %q, want the name", got)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "list"}); err != nil {
		t.Fatalf("secret list: %v", err)
	}
	if !strings.Contains(out.String(), "OPENAI_API_KEY") || !strings.Contains(out.String(), "api.openai.com") || !strings.Contains(out.String(), "mock-OPENAI_API_KEY") {
		t.Errorf("list printed:\n%s", out.String())
	}
	if strings.Contains(out.String(), "sk-live") {
		t.Fatalf("list printed the value:\n%s", out.String())
	}

	// The value lives in exactly one file, and a grep of everything else under the root finds nothing.
	found := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		// The socket under the root is not a file to read, and neither is a directory.
		if err != nil || !d.Type().IsRegular() {
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
	if err := app.Run(t.Context(), []string{"secret", "remove", "OPENAI_API_KEY"}); err != nil {
		t.Fatalf("secret remove: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "OPENAI_API_KEY") {
		t.Errorf("list still lists the removed secret:\n%s", out.String())
	}
}

func TestSecretSetRefusesAnEmptyStdinAndNoDestination(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "\n", &fakeLifecycleRepo{r: &recorder{}})

	err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("set with an empty stdin = %v", err)
	}

	app, _ = newSecretApp(t, &out, "value-123456\n", &fakeLifecycleRepo{r: &recorder{}})

	err = app.Run(t.Context(), []string{"secret", "set", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "no destination") {
		t.Errorf("set with no --destination = %v", err)
	}

	err = app.Run(t.Context(), []string{"secret", "set", "KEY", "--destination", "api.example.com"})
	if err == nil || !strings.Contains(err.Error(), "before the name") {
		t.Errorf("set with the flag after the name = %v", err)
	}
}

func TestSecretSetRefusesToMoveAPlaceholderASandboxHolds(t *testing.T) {
	var out bytes.Buffer

	repo := &fakeLifecycleRepo{r: &recorder{}}
	app, root := newSecretApp(t, &out, "value-654321\n", repo)

	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}
	repo.left = []models.Sandbox{{ID: "sb1", Secrets: []string{"KEY"}}}

	app, _ = newSecretApp(t, &out, "value-654321\n", repo)
	app.Root = root
	err := app.Run(t.Context(), []string{"secret", "set", "--placeholder", "sk_test_moved01", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "sb1") || !strings.Contains(err.Error(), "ungrant") {
		t.Errorf("set that moves a held placeholder = %v", err)
	}

	repo.unreadable = os.ErrPermission
	app, _ = newSecretApp(t, &out, "value-654321\n", repo)
	app.Root = root
	err = app.Run(t.Context(), []string{"secret", "set", "--placeholder", "sk_test_moved01", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("set that moves a placeholder with an unreadable record = %v", err)
	}
}

func TestSecretSetTakesTheValueThreeWaysAndCautionsOnArgv(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "from-stdin-1234\n", &fakeLifecycleRepo{r: &recorder{}})

	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "KEY", "on-the-argv-12"}); err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if !strings.Contains(out.String(), cautionOnArgv) {
		t.Errorf("the argv path printed no caution:\n%s", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "set", "--placeholder", "sk_test_shaped01", "KEY", "-"}); err != nil {
		t.Fatalf("secret set -: %v", err)
	}
	if strings.Contains(out.String(), "caution") {
		t.Errorf("the stdin path printed a caution:\n%s", out.String())
	}

	// The refusal is where the caution matters most: the operator is about to retype the value.
	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "no-dot", "KEY", "on-the-argv-12"}); err == nil || !strings.Contains(err.Error(), "dot") {
		t.Fatalf("a set with a dotless destination = %v, want a refusal that names the missing dot", err)
	}
	if !strings.Contains(out.String(), "caution") {
		t.Errorf("a refused set printed no caution for a value ps already saw:\n%s", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sk_test_shaped01") || strings.Contains(out.String(), "from-stdin") {
		t.Errorf("list printed:\n%s", out.String())
	}
}

// A value that starts with - takes a -- before it, after the name or before it, and a misplaced flag is still refused.
func TestParseSecretSetTakesADashValueAfterADoubleDash(t *testing.T) {
	for _, args := range [][]string{
		{"--destination", "api.example.com", "KEY", "--", "-v4lue"},
		{"--destination", "api.example.com", "--", "KEY", "-v4lue"},
	} {
		opts, err := parseSecretSet(args)
		if err != nil || opts.name != "KEY" || opts.value != "-v4lue" {
			t.Errorf("parseSecretSet(%v) = %+v, %v, want KEY with the value -v4lue", args, opts, err)
		}
	}

	if _, err := parseSecretSet([]string{"KEY", "--destination", "api.example.com"}); err == nil || !strings.Contains(err.Error(), "flags before the name") {
		t.Errorf("a flag after the name returned %v, want the flag order named", err)
	}
}

// A flag after the name is refused rather than stored as the credential, and a -- still lets a value start with -.
func TestParseSecretSetRefusesAFlagAfterTheNameUnlessADoubleDashGuardsIt(t *testing.T) {
	for _, args := range [][]string{
		{"KEY", "--placeholder=sk_test_next01"},
		{"KEY", "--destination=api.example.com"},
		{"KEY", "--dest=api.example.com"},
		{"KEY", "--to=api.example.com"},
		{"KEY", "--"},
		{"KEY", "s3cr3t-value", "--dest=api.example.com"},
		// This -- is the placeholder, not the end of the flags.
		{"--placeholder", "--", "KEY", "--dest=api.example.com"},
	} {
		_, err := parseSecretSet(args)
		if err == nil || !strings.Contains(err.Error(), "flags before the name") || strings.Contains(err.Error(), "example.com") || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("parseSecretSet(%v) = %v, want the flag order named and no argument echoed", args, err)
		}
	}

	for _, args := range [][]string{
		{"--", "KEY", "--placeholder=sk_test_next01"},
		{"KEY", "--", "--placeholder=sk_test_next01"},
	} {
		opts, err := parseSecretSet(args)
		if err != nil || opts.value != "--placeholder=sk_test_next01" {
			t.Errorf("parseSecretSet(%v) = %+v, %v, want the guarded value", args, opts, err)
		}
	}

	opts, err := parseSecretSet([]string{"KEY", "-"})
	if err != nil || opts.valueOnArgv() {
		t.Errorf("parseSecretSet(KEY -) = %+v, %v, want the value from stdin", opts, err)
	}
}

// The verb refuses before it reaches the store, so the value already there survives the typo.
func TestSecretSetKeepsTheStoredValueWhenAFlagFollowsTheName(t *testing.T) {
	var out bytes.Buffer

	app, root := newSecretApp(t, &out, "", &fakeLifecycleRepo{r: &recorder{}})
	store, err := secret.New(filepath.Join(root, "secrets"), func(string) ([]string, error) { return nil, nil })
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	if _, err := store.Set("KEY", "synthetic-before", []string{"api.example.com"}, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := app.Run(t.Context(), []string{"secret", "set", "KEY", "--placeholder=sk_test_next01"}); err == nil {
		t.Fatal("secret set KEY --placeholder=... was taken")
	}

	got, err := store.Value("KEY")
	if err != nil || got != "synthetic-before" {
		t.Errorf("the stored value is %q, %v, want it unchanged", got, err)
	}
}

// The first stop signal cancels ctx, and a pipe whose writer never closes must not hold the verb past it.
func TestSecretSetEndsItsStdinReadOnCancel(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		if err := errors.Join(input.Close(), writer.Close()); err != nil {
			t.Errorf("close the pipe: %v", err)
		}
	})

	var out bytes.Buffer
	app := App{Version: "test", Root: shortRoot(t), Out: &out, Err: &out, in: input}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, []string{"secret", "set", "--destination", "api.example.com", "KEY"}) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled set returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled set still waits on a pipe nobody closed")
	}

	// A goroutine still parked on the pipe would take these bytes before the test does.
	if _, err := writer.WriteString("late\n"); err != nil {
		t.Fatalf("write the pipe: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(input, got); err != nil || string(got) != "late\n" {
		t.Errorf("the pipe gave %q, %v, want what was written after the set returned", got, err)
	}
}

// --dest is the short spelling of --destination, and the two add to one list.
func TestParseSecretSetTakesDestAsDestination(t *testing.T) {
	opts, err := parseSecretSet([]string{"--dest", "api.example.com", "--destination", "uploads.example.com", "KEY"})
	if err != nil || !slices.Equal(opts.destinations, []string{"api.example.com", "uploads.example.com"}) {
		t.Errorf("parseSecretSet = %+v, %v, want both destinations in order", opts, err)
	}
}

// Every other verb echoes the arguments it refused, but one of these is the secret value.
func TestParseSecretSetCountsTooManyArgumentsWithoutTheValue(t *testing.T) {
	_, err := parseSecretSet([]string{"--destination", "api.example.com", "KEY", "s3cr3t-value", "extra"})
	if err == nil || strings.Contains(err.Error(), "s3cr3t-value") || !strings.Contains(err.Error(), "got 3 arguments") {
		t.Errorf("parseSecretSet = %v, want the count of 3 and no value", err)
	}
}

func TestSecretSetRefusesAPlaceholderTheStoreWillNotTake(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "value-123456\n", &fakeLifecycleRepo{r: &recorder{}})

	for _, chosen := range []string{"sk_test", "sk test shaped"} {
		err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "--placeholder", chosen, "KEY"})
		if err == nil {
			t.Errorf("set took the placeholder %q", chosen)
		}
	}
}

func TestSecretListListsTheReadableOnesAndFails(t *testing.T) {
	var out bytes.Buffer

	app, root := newSecretApp(t, &out, "value-123456\n", &fakeLifecycleRepo{r: &recorder{}})

	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "BROKEN"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := app.Run(t.Context(), []string{"secret", "list"})
	if err == nil || !strings.Contains(err.Error(), "BROKEN") {
		t.Errorf("list with a broken file = %v", err)
	}
	if !strings.Contains(out.String(), "KEY") {
		t.Errorf("list did not list the readable secret:\n%s", out.String())
	}

	if err := app.Run(t.Context(), []string{"secret", "remove", "BROKEN"}); err != nil {
		t.Errorf("remove of the broken file = %v", err)
	}
}

func TestSecretRemoveRefusesWhileASandboxHoldsIt(t *testing.T) {
	var out bytes.Buffer

	repo := &fakeLifecycleRepo{r: &recorder{}}
	app, root := newSecretApp(t, &out, "value-123456\n", repo)

	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}
	repo.left = []models.Sandbox{
		{ID: "sandbox1", State: models.StateStopped, Secrets: []string{"KEY"}},
		{ID: "sandbox2", State: models.StateRunning, Secrets: []string{"OTHER"}},
	}

	err := app.Run(t.Context(), []string{"secret", "remove", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || strings.Contains(err.Error(), "sandbox2") {
		t.Errorf("remove = %v, want a refusal naming sandbox1 only", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "KEY")); err != nil {
		t.Errorf("a refused remove removed the secret: %v", err)
	}

	if err := app.Run(t.Context(), []string{"secret", "remove", "--force", "KEY"}); err != nil {
		t.Errorf("remove --force = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "KEY")); err == nil {
		t.Error("remove --force left the secret")
	}
}

func TestSecretRemoveRefusesWhenARecordIsUnreadable(t *testing.T) {
	var out bytes.Buffer

	repo := &fakeLifecycleRepo{r: &recorder{}}
	app, _ := newSecretApp(t, &out, "value-123456\n", repo)

	if err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "KEY"}); err != nil {
		t.Fatal(err)
	}
	repo.unreadable = os.ErrPermission

	err := app.Run(t.Context(), []string{"secret", "remove", "KEY"})
	if err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("remove with an unreadable record = %v", err)
	}
}

func TestSecretRemoveOfAMissingSecretFails(t *testing.T) {
	var out bytes.Buffer

	app, _ := newSecretApp(t, &out, "", &fakeLifecycleRepo{r: &recorder{}})

	err := app.Run(t.Context(), []string{"secret", "remove", "NOPE"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("remove of a missing secret = %v", err)
	}
}

// grantApp puts a daemon up over a sandbox whose bundle is on disk, which is what a grant edits.
func grantApp(t *testing.T, out *bytes.Buffer, state models.State) (App, *fakeLifecycleRepo, bundle.Bundle) {
	t.Helper()

	root := shortRoot(t)

	secrets, err := secret.New(filepath.Join(root, "secrets"), nil)
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	if _, err := secrets.Set("TOKEN", "s3cr3t-value", []string{"api.example.com"}, ""); err != nil {
		t.Fatalf("secrets.Set: %v", err)
	}

	policies, err := egress.NewStore(filepath.Join(root, "policies"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	repo := &fakeLifecycleRepo{r: &recorder{}, sb: models.Sandbox{ID: "sandbox1", Name: "web", State: state}, stateDir: t.TempDir()}

	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/ssl/certs/ca-certificates.crt"), []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	builder, err := bundle.New("/usr/local/bin/shard-init")
	if err != nil {
		t.Fatalf("bundle.New: %v", err)
	}
	b, err := builder.Build(runspec.Resolve(models.SandboxSpec{ID: "sandbox1", StateDir: repo.stateDir, RootFS: rootfs},
		models.ImageConfig{}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	daemon := &fakeDaemon{
		app:       App{Root: root},
		repoSvc:   repo,
		secretSvc: secrets,
		policySvc: policies,
		netSvc:    &fakeLifecycleNet{r: repo.r},
		proxyCA:   []byte("-----BEGIN CERTIFICATE-----\nproxy-ca\n-----END CERTIFICATE-----\n"),
	}
	serveDaemon(t, daemon)

	return App{Version: "test", Root: root, Out: out, Err: out}, repo, b
}

func TestSecretGrantAndUngrantRoundTrip(t *testing.T) {
	var out bytes.Buffer

	app, repo, b := grantApp(t, &out, models.StateStopped)

	if err := app.Run(t.Context(), []string{"secret", "grant", "web", "TOKEN"}); err != nil {
		t.Fatalf("secret grant: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("grant printed %q, want the sandbox id", got)
	}
	if !slices.Contains(repo.sb.Secrets, "TOKEN") {
		t.Errorf("the record holds %v, want the grant", repo.sb.Secrets)
	}

	rt, err := b.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rt.Env, "TOKEN=mock-TOKEN") {
		t.Errorf("the guest environment is %v, want the placeholder", rt.Env)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"secret", "ungrant", "web", "TOKEN"}); err != nil {
		t.Fatalf("secret ungrant: %v", err)
	}
	if len(repo.sb.Secrets) != 0 {
		t.Errorf("the record still holds %v", repo.sb.Secrets)
	}

	rt, err = b.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rt.Env, "TOKEN=mock-TOKEN") {
		t.Errorf("the guest environment still holds the placeholder: %v", rt.Env)
	}
}

func TestSecretGrantRefusesARunningSandboxAndAMissingSecret(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := grantApp(t, &out, models.StateRunning)

	err := app.Run(t.Context(), []string{"secret", "grant", "web", "TOKEN"})
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("the grant of a running sandbox = %v", err)
	}

	app, _, _ = grantApp(t, &out, models.StateStopped)

	err = app.Run(t.Context(), []string{"secret", "grant", "web", "MISSING"})
	if err == nil || !strings.Contains(err.Error(), "shard secret set") {
		t.Errorf("the grant of a secret the store does not hold = %v", err)
	}
}

func TestParseSecretGrantRefusesTheWrongArguments(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := grantApp(t, &out, models.StateStopped)

	for _, args := range [][]string{
		{"secret", "grant", "web"},
		{"secret", "grant", "web", "TOKEN", "OTHER"},
		{"secret", "ungrant", "--force", "web", "TOKEN"},
	} {
		if err := app.Run(t.Context(), args); err == nil {
			t.Errorf("%v was taken", args)
		}
	}
}

func TestSecretSetRefusesABadNameBeforeItAsksTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, root := newSecretApp(t, &out, "value-123456\n", &fakeLifecycleRepo{r: &recorder{}})

	// A mistyped set hands the value as the name, so the refusal must not echo it and must write nothing.
	err := app.Run(t.Context(), []string{"secret", "set", "--destination", "api.example.com", "sk-live-abcdef123456"})
	if err == nil || !strings.Contains(err.Error(), "environment variable name") {
		t.Fatalf("set with a bad name = %v", err)
	}
	if strings.Contains(err.Error(), "sk-live-abcdef123456") {
		t.Errorf("the refusal echoes the name it refused: %v", err)
	}

	entries, readErr := os.ReadDir(filepath.Join(root, "secrets"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused set wrote %d files", len(entries))
	}
}
