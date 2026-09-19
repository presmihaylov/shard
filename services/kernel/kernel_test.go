package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestVersionMatchesPackaging pins the Go constant to the Makefile's, so one bump cannot land without the other.
func TestVersionMatchesPackaging(t *testing.T) {
	mk, err := os.ReadFile(filepath.Join("..", "..", "packaging", "kernel", "version.mk"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^KERNEL_VERSION = (\S+)$`).FindSubmatch(mk)
	if m == nil {
		t.Fatal("no KERNEL_VERSION in version.mk")
	}
	if got := string(m[1]); got != Version {
		t.Fatalf("version.mk says %s, kernel.Version says %s", got, Version)
	}
}

func TestEnsureDownloadsOnceAndVerifies(t *testing.T) {
	body := []byte("not a kernel")
	artifacts["test"] = struct{ name, sha256 string }{"Image-test", sum(body)}
	defer delete(artifacts, "test")

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteTo(srv.URL, client.Transport)

	root := t.TempDir()
	s := New(root, WithHTTPClient(client))
	k, err := s.Ensure(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if k.SHA256 != sum(body) || k.Version != Version || k.Arch != "test" {
		t.Fatalf("unexpected kernel %+v", k)
	}
	if _, err := s.Ensure(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("downloaded %d times, want 1", hits)
	}

	if err := os.WriteFile(k.Path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(context.Background(), "test"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("tampered file passed: %v", err)
	}
}

func TestConcurrentFirstUseDownloadsOnce(t *testing.T) {
	body := []byte("not a kernel")
	artifacts["test"] = struct{ name, sha256 string }{"Image-test", sum(body)}
	defer delete(artifacts, "test")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteTo(srv.URL, client.Transport)

	s := New(t.TempDir(), WithHTTPClient(client))
	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := s.Ensure(context.Background(), "test")
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("downloaded %d times, want 1", got)
	}
}

func TestEnsureRefusesABadDownload(t *testing.T) {
	artifacts["test"] = struct{ name, sha256 string }{"Image-test", sum([]byte("expected"))}
	defer delete(artifacts, "test")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("something else"))
	}))
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteTo(srv.URL, client.Transport)

	root := t.TempDir()
	_, err := New(root, WithHTTPClient(client)).Ensure(context.Background(), "test")
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("bad download passed: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "kernel", Tag())); len(entries) != 0 {
		t.Fatalf("left %d files behind", len(entries))
	}
}

func TestWithLocalStillChecksTheHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Image")
	body := []byte("dev kernel")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	k, err := New(t.TempDir(), WithLocal(path, sum(body))).Ensure(context.Background(), "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if k.Path != path {
		t.Fatalf("path %s, want %s", k.Path, path)
	}

	_, err = New(t.TempDir(), WithLocal(path, sum([]byte("other")))).Ensure(context.Background(), "arm64")
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("wrong local hash passed: %v", err)
	}
}

// rewriteTo sends every request to the test server whatever host the release URL names.
func rewriteTo(base string, next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		u := *r.URL
		u.Scheme, u.Host = "http", base[len("http://"):]
		r2 := r.Clone(r.Context())
		r2.URL = &u
		r2.Host = u.Host
		return next.RoundTrip(r2)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFromEnvNeedsBoth(t *testing.T) {
	t.Setenv(LocalEnv, "/k")
	t.Setenv(LocalSHA256Env, "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("a path without a hash passed")
	}

	t.Setenv(LocalSHA256Env, "abc")
	opts, err := FromEnv()
	if err != nil || len(opts) != 1 {
		t.Fatalf("opts %v, err %v", opts, err)
	}
}
