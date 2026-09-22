package kernel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// The daemon fetches under its lock, so a release endpoint that never answers must give the context's end back, not hang.
func TestEnsureStopsABlockedDownloadWhenTheContextEnds(t *testing.T) {
	artifacts["test"] = struct{ name, sha256 string }{"Image-test", sum([]byte("never"))}
	defer delete(artifacts, "test")

	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-released
	}))
	defer srv.Close()
	defer close(released)
	client := srv.Client()
	client.Transport = rewriteTo(srv.URL, client.Transport)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := New(t.TempDir(), WithHTTPClient(client)).Ensure(ctx, "test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure = %v, want the context's deadline", err)
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

func TestEnsureLogsTheFetchAndEveryVerifiedKernel(t *testing.T) {
	body := []byte("a kernel")
	artifacts["test"] = struct{ name, sha256 string }{"Image-test", sum(body)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	url, err := URL("test")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := New(t.TempDir(), WithHTTPClient(&http.Client{Transport: rewriteTo(srv.URL, http.DefaultTransport)}), WithLogger(log.New(&out, "", 0)))
	k, err := s.Ensure(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	want := "kernel: downloading " + url + " to " + k.Path + "\nkernel: verified " + k.Path + " sha256 " + sum(body) + "\n"
	if out.String() != want {
		t.Fatalf("log:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	if _, err := s.Ensure(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "kernel: verified "+k.Path+" sha256 "+sum(body)+"\n"; got != want {
		t.Fatalf("a cached kernel logged %q, want %q", got, want)
	}
}

func TestEnsureLogsAChecksumMismatch(t *testing.T) {
	artifacts["test"] = struct{ name, sha256 string }{"Image-test", sum([]byte("a kernel"))}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("something else"))
	}))
	defer srv.Close()
	url, err := URL("test")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := New(t.TempDir(), WithHTTPClient(&http.Client{Transport: rewriteTo(srv.URL, http.DefaultTransport)}), WithLogger(log.New(&out, "", 0)))
	if _, err := s.Ensure(context.Background(), "test"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if !strings.Contains(out.String(), "kernel: checksum mismatch for "+url+": got "+sum([]byte("something else"))+", want "+sum([]byte("a kernel"))) {
		t.Fatalf("log:\n%s", out.String())
	}
}

// TestConfigsCarryWhatAGuestMounts pins the filesystems both substrates need, so a config regenerated from a defconfig cannot drop one.
func TestConfigsCarryWhatAGuestMounts(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		cfg, err := os.ReadFile(filepath.Join("..", "..", "packaging", "kernel", "config-"+arch))
		if err != nil {
			t.Fatal(err)
		}
		for _, opt := range []string{"CONFIG_EROFS_FS", "CONFIG_EROFS_FS_XATTR", "CONFIG_OVERLAY_FS", "CONFIG_EXT4_FS", "CONFIG_VIRTIO_VSOCKETS"} {
			if !bytes.Contains(cfg, []byte("\n"+opt+"=y\n")) {
				t.Errorf("config-%s lacks %s=y", arch, opt)
			}
		}
	}
}
