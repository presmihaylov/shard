package bundle

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newEnvBundle lays out a bundle by hand, so the tests need no image and no Linux.
func newEnvBundle(t *testing.T, env []string) Bundle {
	t.Helper()

	stateDir := t.TempDir()
	rootFS := t.TempDir()

	b, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	config := `{"process":{"args":["/init"],"cwd":"/","env":["` + strings.Join(env, `","`) + `"]},` +
		`"annotations":{"` + rootfsAnnotation + `":"` + rootFS + `"}}`
	if err := os.WriteFile(filepath.Join(b.Dir, "config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	return b
}

func envOf(t *testing.T, b Bundle) []string {
	t.Helper()

	rt, err := b.Runtime()
	if err != nil {
		t.Fatal(err)
	}

	return rt.Env
}

func TestSetEnvAddsOnceAndRefusesACollision(t *testing.T) {
	b := newEnvBundle(t, []string{"PATH=/usr/bin"})

	if err := b.SetEnv("API_KEY", "mock-API_KEY"); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}
	if err := b.SetEnv("API_KEY", "mock-API_KEY"); err != nil {
		t.Fatalf("a retried SetEnv: %v", err)
	}

	env := envOf(t, b)
	if !slices.Equal(env, []string{"PATH=/usr/bin", "API_KEY=mock-API_KEY"}) {
		t.Errorf("env = %v", env)
	}

	err := b.SetEnv("PATH", "/other")
	if err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Errorf("SetEnv over a held name = %v", err)
	}
}

func TestRemoveEnvTakesTheEntryOutAndIsIdempotent(t *testing.T) {
	b := newEnvBundle(t, []string{"PATH=/usr/bin", "API_KEY=mock-API_KEY"})

	if err := b.RemoveEnv("API_KEY"); err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	if err := b.RemoveEnv("API_KEY"); err != nil {
		t.Fatalf("a second RemoveEnv: %v", err)
	}

	if env := envOf(t, b); !slices.Equal(env, []string{"PATH=/usr/bin"}) {
		t.Errorf("env = %v", env)
	}
}

func TestTrustProxyPlantsTheCAAndTheEnv(t *testing.T) {
	b := newEnvBundle(t, []string{"PATH=/usr/bin", "SSL_CERT_FILE=/mine.crt"})

	rt, err := b.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	rootsPath := filepath.Join(rt.RootFS, guestCABundle)
	if err := os.MkdirAll(filepath.Dir(rootsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootsPath, []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.TrustProxy([]byte("proxy-ca\n")); err != nil {
		t.Fatalf("TrustProxy: %v", err)
	}

	planted, err := os.ReadFile(filepath.Join(b.Upper, guestCABundle))
	if err != nil || string(planted) != "image-roots\nproxy-ca\n" {
		t.Errorf("the planted bundle = %q, %v", planted, err)
	}

	env := envOf(t, b)
	if !slices.Contains(env, "REQUESTS_CA_BUNDLE=/"+guestCABundle) {
		t.Errorf("env %v lacks the CA variables", env)
	}
	// What the user set stays, per the create-time rule.
	if !slices.Contains(env, "SSL_CERT_FILE=/mine.crt") || slices.Contains(env, "SSL_CERT_FILE=/"+guestCABundle) {
		t.Errorf("env %v overrode the user's SSL_CERT_FILE", env)
	}
}
