package bundle_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/runspec"
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
	spec := newSpec(t)
	write(t, filepath.Join(spec.RootFS, "mine.pem"), "-----MY ROOT-----\n")
	spec.ProxyCA = []byte("ca\n")
	spec.Env = []string{"SSL_CERT_FILE=/mine.pem"}
	b, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if !slices.Contains(got.Process.Env, "SSL_CERT_FILE=/mine.pem") || slices.Contains(got.Process.Env, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") {
		t.Errorf("the user's SSL_CERT_FILE was replaced: %v", got.Process.Env)
	}
	if bundle := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); bundle != "-----MY ROOT-----\nca\n" {
		t.Errorf("the guest bundle reads %q", bundle)
	}
}

// A RHEL family image keeps its roots elsewhere; the written file must still start with them.
func TestBuildKeepsTheRootsOfAUBIShapedImage(t *testing.T) {
	spec := newSpec(t)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/pki/tls/certs"), 0o755); err != nil {
		t.Fatalf("create the image certs dir: %v", err)
	}
	write(t, filepath.Join(spec.RootFS, "etc/pki/tls/certs/ca-bundle.crt"), "-----UBI ROOT-----\n")
	spec.ProxyCA = []byte("-----PROXY CA-----\n")

	b, _ := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if bundle := readFile(t, filepath.Join(b.Upper, "etc/ssl/certs/ca-certificates.crt")); bundle != "-----UBI ROOT-----\n-----PROXY CA-----\n" {
		t.Errorf("the guest bundle reads %q", bundle)
	}
}

// The env points every library at the written file, so a bundle of the proxy CA alone would drop the public roots.
func TestBuildRefusesAnImageThatShipsNoRoots(t *testing.T) {
	spec := newSpec(t)
	spec.ProxyCA = []byte("-----PROXY CA-----\n")
	spec = runspec.Resolve(spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	_, err := newService(t).Build(spec)
	if err == nil {
		t.Fatal("Build wrote a bundle that holds the proxy CA alone")
	}
	for _, want := range []string{spec.RootFS, "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %s", err, want)
		}
	}
}

// An image can point the bundle path at a host file, and a secret record is one: the read must not follow it out.
func TestBuildRefusesAnImageRootsLinkOutsideTheRootFS(t *testing.T) {
	spec := newSpec(t)
	outside := filepath.Join(t.TempDir(), "secret.json")
	write(t, outside, `{"value":"sk-real"}`)
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatalf("create the image certs dir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt")); err != nil {
		t.Fatalf("link the bundle outside: %v", err)
	}
	spec.ProxyCA = []byte("-----PROXY CA-----\n")
	spec = runspec.Resolve(spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	_, err := newService(t).Build(spec)
	if err == nil {
		t.Fatal("Build followed the image bundle link out of the rootfs")
	}
	if strings.Contains(err.Error(), "sk-real") {
		t.Errorf("the error carries the file: %v", err)
	}
}
