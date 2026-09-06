package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/secret"
)

const imageRoots = "-----BEGIN CERTIFICATE-----\nimage-root\n-----END CERTIFICATE-----\n"

// granted wires the service over a stopped sandbox whose bundle is on disk, which is what a grant edits.
func granted(t *testing.T, r *recorder, sb models.Sandbox, env ...string) (*sandbox.Service, layers, bundle.Bundle) {
	t.Helper()

	svc, l := newService(t, r, sb)
	l.repo.stateDir = t.TempDir()

	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/ssl/certs/ca-certificates.crt"), []byte(imageRoots), 0o644); err != nil {
		t.Fatal(err)
	}

	builder, err := bundle.New("/usr/local/bin/shard-init")
	if err != nil {
		t.Fatalf("bundle.New: %v", err)
	}

	spec := runspec.Resolve(models.SandboxSpec{ID: sb.ID, StateDir: l.repo.stateDir, RootFS: rootfs, Env: env},
		models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	b, err := builder.Build(spec)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := l.secrets.Set("TOKEN", "s3cr3t", []string{"api.example.com"}, ""); err != nil {
		t.Fatalf("secrets.Set: %v", err)
	}

	return svc, l, b
}

func guestEnv(t *testing.T, b bundle.Bundle, name string) (string, bool) {
	t.Helper()

	rt, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	for _, entry := range rt.Env {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			return value, true
		}
	}

	return "", false
}

func TestGrantSecretPlantsThePlaceholderTheCAAndTheRecord(t *testing.T) {
	r := &recorder{}
	svc, l, b := granted(t, r, stopped())

	sb, err := svc.GrantSecret(context.Background(), "web", "TOKEN")
	if err != nil {
		t.Fatalf("GrantSecret: %v", err)
	}

	if !slices.Contains(sb.Secrets, "TOKEN") {
		t.Errorf("the record holds %v, want the grant", sb.Secrets)
	}
	if value, _ := guestEnv(t, b, "TOKEN"); value != secret.DefaultPlaceholder("TOKEN") {
		t.Errorf("$TOKEN = %q, want the placeholder", value)
	}

	trusted, _ := guestEnv(t, b, "SSL_CERT_FILE")
	if trusted != "/etc/ssl/certs/ca-certificates.crt" {
		t.Errorf("SSL_CERT_FILE = %q, want the path the image reads", trusted)
	}
	merged, err := os.ReadFile(filepath.Join(b.Upper, trusted))
	if err != nil {
		t.Fatalf("read the merged CA bundle: %v", err)
	}
	if !strings.HasPrefix(string(merged), imageRoots) || len(merged) == len(imageRoots) {
		t.Errorf("the merged bundle is not the image roots plus the proxy CA:\n%s", merged)
	}

	// The grant fronts the sandbox, so the host must send its web ports to the proxy.
	if !slices.Contains(r.calls, "net.Reapply") {
		t.Errorf("the grant did not reapply the host rules: %v", r.calls)
	}
	if !slices.Contains(l.repo.sb.Secrets, "TOKEN") {
		t.Errorf("the stored record holds %v", l.repo.sb.Secrets)
	}
}

func TestGrantSecretTakesASecondCall(t *testing.T) {
	svc, _, b := granted(t, &recorder{}, stopped())

	if _, err := svc.GrantSecret(context.Background(), "sandbox1", "TOKEN"); err != nil {
		t.Fatalf("GrantSecret: %v", err)
	}
	if _, err := svc.GrantSecret(context.Background(), "sandbox1", "TOKEN"); err != nil {
		t.Fatalf("the second GrantSecret: %v", err)
	}

	if value, _ := guestEnv(t, b, "TOKEN"); value != secret.DefaultPlaceholder("TOKEN") {
		t.Errorf("$TOKEN = %q after two grants", value)
	}
}

func TestGrantSecretRefusesARunningSandbox(t *testing.T) {
	svc, _, _ := granted(t, &recorder{}, running())

	_, err := svc.GrantSecret(context.Background(), "sandbox1", "TOKEN")
	if err == nil {
		t.Fatal("the grant took a running sandbox")
	}
	if !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("the error does not name the fix: %v", err)
	}
}

func TestGrantSecretRefusesAPausedSandbox(t *testing.T) {
	svc, _, _ := granted(t, &recorder{}, pausedSandbox())

	if _, err := svc.GrantSecret(context.Background(), "sandbox1", "TOKEN"); err == nil {
		t.Fatal("the grant took a paused sandbox, whose snapshot already holds the environment")
	}
}

func TestGrantSecretRefusesASecretThatDoesNotExist(t *testing.T) {
	svc, _, b := granted(t, &recorder{}, stopped())
	before := config(t, b)

	_, err := svc.GrantSecret(context.Background(), "sandbox1", "MISSING")
	if err == nil {
		t.Fatal("the grant took a secret the store does not hold")
	}
	if !strings.Contains(err.Error(), "shard secret set") {
		t.Errorf("the error does not name the fix: %v", err)
	}
	if config(t, b) != before {
		t.Error("the refused grant rewrote config.json")
	}
}

func TestGrantSecretRefusesANameTheGuestHoldsAndWritesNothing(t *testing.T) {
	svc, l, b := granted(t, &recorder{}, stopped(), "TOKEN=of-its-own")
	before := config(t, b)

	_, err := svc.GrantSecret(context.Background(), "sandbox1", "TOKEN")
	if err == nil {
		t.Fatal("the grant took a name the guest environment already holds")
	}

	if config(t, b) != before {
		t.Error("the refused grant rewrote config.json")
	}
	// The CA is the other write a grant makes, and a refused one must not have planted it either.
	if _, err := os.Stat(filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); !os.IsNotExist(err) {
		t.Errorf("the refused grant wrote the writable layer: %v", err)
	}
	if len(l.repo.sb.Secrets) != 0 {
		t.Errorf("the record holds %v after a refused grant", l.repo.sb.Secrets)
	}
}

func TestUngrantSecretTakesThePlaceholderBackAndLeavesTheCA(t *testing.T) {
	svc, l, b := granted(t, &recorder{}, stopped())

	if _, err := svc.GrantSecret(context.Background(), "sandbox1", "TOKEN"); err != nil {
		t.Fatalf("GrantSecret: %v", err)
	}

	sb, err := svc.UngrantSecret(context.Background(), "sandbox1", "TOKEN")
	if err != nil {
		t.Fatalf("UngrantSecret: %v", err)
	}

	if slices.Contains(sb.Secrets, "TOKEN") {
		t.Errorf("the record still holds %v", sb.Secrets)
	}
	if _, held := guestEnv(t, b, "TOKEN"); held {
		t.Error("the guest environment still holds the placeholder")
	}
	if _, held := guestEnv(t, b, "SSL_CERT_FILE"); !held {
		t.Error("the ungrant took the proxy CA away")
	}
	if len(l.repo.sb.Secrets) != 0 {
		t.Errorf("the stored record holds %v", l.repo.sb.Secrets)
	}

	// A second ungrant changes nothing, so an interrupted one is finished by a retry.
	if _, err := svc.UngrantSecret(context.Background(), "sandbox1", "TOKEN"); err != nil {
		t.Fatalf("the second UngrantSecret: %v", err)
	}
}

func TestSecretHoldersNamesEverySandboxThatGrantsIt(t *testing.T) {
	r := &recorder{}
	_, l, _ := granted(t, r, stopped())
	l.repo.left = []models.Sandbox{
		{ID: "sandbox1", Secrets: []string{"TOKEN"}},
		{ID: "sandbox2"},
		{ID: "sandbox3", Secrets: []string{"OTHER", "TOKEN"}},
	}

	holders, err := sandbox.SecretHolders(l.repo, "TOKEN")
	if err != nil {
		t.Fatalf("SecretHolders: %v", err)
	}
	if !slices.Equal(holders, []string{"sandbox1", "sandbox3"}) {
		t.Errorf("SecretHolders = %v", holders)
	}
}

func config(t *testing.T, b bundle.Bundle) string {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(b.Dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}

	return string(blob)
}
