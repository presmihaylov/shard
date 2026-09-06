package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

const (
	imageRoots = "-----BEGIN CERTIFICATE-----\nimage-root\n-----END CERTIFICATE-----\n"
	proxyCA    = "-----BEGIN CERTIFICATE-----\nproxy-ca\n-----END CERTIFICATE-----\n"
)

func envOf(t *testing.T, env []string, name string) string {
	t.Helper()

	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && key == name {
			return value
		}
	}

	t.Fatalf("the environment %v has no %s", env, name)

	return ""
}

// A UBI image names its bundle in SSL_CERT_FILE and keeps it under /etc/pki, and the guest must go on reading
// its own roots from that same path, with the proxy CA appended.
func TestBuildPlantsTheProxyCABesideTheImageRoots(t *testing.T) {
	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/pki/tls/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/pki/tls/certs/ca-bundle.crt"), imageRoots)
	spec.ProxyCA = []byte(proxyCA)

	b, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, Env: []string{"SSL_CERT_FILE=/etc/pki/tls/certs/ca-bundle.crt"}})

	named := envOf(t, got.Process.Env, "SSL_CERT_FILE")
	if named != "/etc/pki/tls/certs/ca-bundle.crt" {
		t.Errorf("SSL_CERT_FILE moved to %q, want the path the image already reads", named)
	}
	for _, key := range []string{"REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if envOf(t, got.Process.Env, key) != named {
			t.Errorf("%s does not point at %s: %v", key, named, got.Process.Env)
		}
	}

	merged := readFile(t, filepath.Join(b.Upper, named))
	if !strings.HasPrefix(merged, imageRoots) {
		t.Errorf("the merged bundle lost the image roots:\n%s", merged)
	}
	if !strings.HasSuffix(merged, proxyCA) {
		t.Errorf("the merged bundle does not end with the proxy CA:\n%s", merged)
	}
}

func TestBuildFindsTheDebianRootsWithoutAnEnv(t *testing.T) {
	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), imageRoots)
	spec.ProxyCA = []byte(proxyCA)

	b, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if envOf(t, got.Process.Env, "SSL_CERT_FILE") != "/etc/ssl/certs/ca-certificates.crt" {
		t.Errorf("SSL_CERT_FILE = %v", got.Process.Env)
	}
	if got := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}
}

// A bundle holding the proxy CA alone would make the guest trust nothing but the proxy, so no bundle is a refusal.
func TestBuildRefusesToFrontAnImageWithNoRoots(t *testing.T) {
	spec := newSpec(t)
	spec.Entrypoint = []string{"/bin/sh"}
	spec.ProxyCA = []byte(proxyCA)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty file is what a stripped image ships, and it counts as no bundle.
	write(t, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), "")

	_, err := newService(t).Build(spec)
	if err == nil {
		t.Fatal("Build fronted an image with no CA bundle")
	}
	for _, want := range append([]string{spec.RootFS}, "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem") {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}

	if _, err := os.Stat(filepath.Join(spec.StateDir, "overlay/upper/etc/ssl/certs/ca-certificates.crt")); err == nil {
		t.Error("a refused build wrote a bundle into the writable layer")
	}
}

func TestBuildReadsTheRootsInsideTheRootfsOnly(t *testing.T) {
	spec := newSpec(t)
	spec.Entrypoint = []string{"/bin/sh"}
	spec.ProxyCA = []byte(proxyCA)
	outside := filepath.Join(t.TempDir(), "host-roots.crt")
	write(t, outside, imageRoots)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt")); err != nil {
		t.Fatal(err)
	}

	if _, err := newService(t).Build(spec); err == nil {
		t.Error("Build followed a symlink out of the rootfs")
	}
}

func TestTrustsUserRefusesTheTrustVariables(t *testing.T) {
	if err := bundle.TrustsUser([]string{"LANG=C", "TZ=UTC"}); err != nil {
		t.Errorf("a plain environment was refused: %v", err)
	}
	for _, key := range bundle.TrustEnv {
		err := bundle.TrustsUser([]string{"LANG=C", key + "=/tmp/mine.pem"})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("--env %s on a fronted sandbox = %v, want a refusal naming it", key, err)
		}
	}
}

func TestBuildPlantsNothingWhenNotFronted(t *testing.T) {
	b, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	for _, entry := range got.Process.Env {
		if strings.HasPrefix(entry, "SSL_CERT_FILE=") {
			t.Errorf("an unfronted sandbox got %s", entry)
		}
	}
	if _, err := os.Stat(filepath.Join(b.Upper, "etc/ssl")); err == nil {
		t.Error("an unfronted sandbox got a bundle in its writable layer")
	}
}
