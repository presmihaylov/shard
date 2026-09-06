package bundle_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// built is one bundle on disk, as a create left it, which the grant verbs then edit.
func built(t *testing.T, env ...string) bundle.Bundle {
	t.Helper()

	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), imageRoots)
	spec.Env = env

	b, _ := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	return b
}

func runtimeEnv(t *testing.T, b bundle.Bundle) []string {
	t.Helper()

	rt, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	return rt.Env
}

func TestSetEnvAddsTheVariableTheNextStartReads(t *testing.T) {
	b := built(t)

	if err := b.CanSetEnv("TOKEN"); err != nil {
		t.Fatalf("CanSetEnv: %v", err)
	}
	if err := b.SetEnv("TOKEN", "mock-TOKEN"); err != nil {
		t.Fatalf("SetEnv: %v", err)
	}

	if got := envOf(t, runtimeEnv(t, b), "TOKEN"); got != "mock-TOKEN" {
		t.Errorf("TOKEN = %q, want the placeholder", got)
	}
}

func TestSetEnvRefusesAVariableTheGuestHolds(t *testing.T) {
	b := built(t, "TOKEN=already")

	if err := b.CanSetEnv("TOKEN"); err == nil {
		t.Fatal("CanSetEnv took a variable the guest already holds")
	}

	before := readFile(t, filepath.Join(b.Dir, "config.json"))

	if err := b.SetEnv("TOKEN", "mock-TOKEN"); err == nil {
		t.Fatal("SetEnv took a variable the guest already holds")
	}

	// A refused grant must leave the bundle as it found it, byte for byte.
	if after := readFile(t, filepath.Join(b.Dir, "config.json")); after != before {
		t.Error("the refused SetEnv rewrote config.json")
	}
}

func TestRemoveEnvDropsTheVariableAndTakesASecondCall(t *testing.T) {
	b := built(t, "TOKEN=mock-TOKEN")

	if err := b.RemoveEnv("TOKEN"); err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	if slices.ContainsFunc(runtimeEnv(t, b), func(entry string) bool { return strings.HasPrefix(entry, "TOKEN=") }) {
		t.Errorf("TOKEN survived the remove: %v", runtimeEnv(t, b))
	}

	before := readFile(t, filepath.Join(b.Dir, "config.json"))
	if err := b.RemoveEnv("TOKEN"); err != nil {
		t.Fatalf("the second RemoveEnv: %v", err)
	}
	if after := readFile(t, filepath.Join(b.Dir, "config.json")); after != before {
		t.Error("a remove of a variable the bundle does not hold rewrote config.json")
	}
}

func TestTrustProxyPlantsTheCALateAndNeverTwice(t *testing.T) {
	b := built(t)

	if err := b.TrustProxy([]byte(proxyCA)); err != nil {
		t.Fatalf("TrustProxy: %v", err)
	}

	named := envOf(t, runtimeEnv(t, b), "SSL_CERT_FILE")
	if named != "/etc/ssl/certs/ca-certificates.crt" {
		t.Errorf("SSL_CERT_FILE = %q, want the path the image already reads", named)
	}
	for _, key := range []string{"REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if envOf(t, runtimeEnv(t, b), key) != named {
			t.Errorf("%s does not point at %s", key, named)
		}
	}
	if got := readFile(t, filepath.Join(b.Upper, named)); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}

	// Every later grant plants again, and it must re-merge from the image roots rather than append to its own output.
	for plant := 2; plant <= 4; plant++ {
		if err := b.TrustProxy([]byte(proxyCA)); err != nil {
			t.Fatalf("TrustProxy %d: %v", plant, err)
		}
		if got := readFile(t, filepath.Join(b.Upper, named)); got != imageRoots+proxyCA {
			t.Errorf("plant %d changed the merged bundle:\n%s", plant, got)
		}
	}
}

func TestTrustProxyRefusesAnImageWithNoRoots(t *testing.T) {
	spec := newSpec(t)
	b, _ := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	err := b.TrustProxy([]byte(proxyCA))
	if err == nil {
		t.Fatal("TrustProxy planted the proxy CA alone")
	}
	if !strings.Contains(err.Error(), "no CA bundle") {
		t.Errorf("the error does not name what is missing: %v", err)
	}
}
