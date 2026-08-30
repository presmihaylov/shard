package bundle_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// The guest verifies every host against one file: what the image shipped, then the proxy CA.
func TestBuildAppendsTheProxyCAToTheImageRoots(t *testing.T) {
	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatalf("create the image certs dir: %v", err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), "-----IMAGE ROOT-----")
	spec.ProxyCA = []byte("-----PROXY CA-----\n")

	b, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if bundle := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); bundle != "-----IMAGE ROOT-----\n-----PROXY CA-----\n" {
		t.Errorf("the guest bundle reads %q", bundle)
	}

	for _, want := range []string{"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt", "REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt", "NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt"} {
		if !slices.Contains(got.Process.Env, want) {
			t.Errorf("the environment lacks %s: %v", want, got.Process.Env)
		}
	}
}

func TestBuildWritesNoRootsWhenNothingFrontsTheSandbox(t *testing.T) {
	b, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if _, err := os.Stat(filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); err == nil {
		t.Error("a sandbox the proxy does not front got a CA bundle")
	}
	if slices.ContainsFunc(got.Process.Env, func(e string) bool { return strings.HasPrefix(e, "SSL_CERT_FILE=") }) {
		t.Error("a sandbox the proxy does not front got SSL_CERT_FILE")
	}
}

func TestBuildKeepsTheUsersOwnCAVariable(t *testing.T) {
	spec := models.SandboxSpec{ProxyCA: []byte("ca\n"), Env: []string{"SSL_CERT_FILE=/mine.pem"}}
	_, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if !slices.Contains(got.Process.Env, "SSL_CERT_FILE=/mine.pem") || slices.Contains(got.Process.Env, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("the user's SSL_CERT_FILE was replaced: %v", got.Process.Env)
	}
}
