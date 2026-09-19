// Package kernel fetches the guest kernel a microVM substrate boots and refuses one whose checksum moved.
package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Version is the Linux release packaging/kernel builds; packaging/kernel/version.mk must say the same.
const Version = "6.12.110"

// Build counts the shard builds of that release, so a config change ships without a kernel bump.
const Build = 1

// ErrChecksum marks a kernel file whose bytes do not hash to the value shard was built with.
var ErrChecksum = errors.New("kernel checksum mismatch")

// Kernel is one verified kernel file, ready to boot.
type Kernel struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
}

// artifacts is the hash of every release file, the output of make kernel-reproducible at this Version and Build.
var artifacts = map[string]struct{ name, sha256 string }{
	"arm64": {"Image-arm64", "ef2b1ec110009c6e791f048c7069afd7afa88d51a70de56fddc9414ccde9bbed"},
	"amd64": {"vmlinux-amd64", "13fc80b85d189cd85f5c4553bd80737230ca853d3fe635e6f8cd154f0676b029"},
}

// Tag is the GitHub release the artifacts live under.
func Tag() string { return fmt.Sprintf("kernel-%s-%d", Version, Build) }

// SHA256 is the hash the release file for arch must have.
func SHA256(arch string) (string, error) {
	a, ok := artifacts[arch]
	if !ok {
		return "", fmt.Errorf("no guest kernel for arch %q", arch)
	}

	return a.sha256, nil
}

// URL is where Ensure downloads the kernel for arch from.
func URL(arch string) (string, error) {
	a, ok := artifacts[arch]
	if !ok {
		return "", fmt.Errorf("no guest kernel for arch %q", arch)
	}

	return "https://github.com/presmihaylov/shard/releases/download/" + Tag() + "/" + a.name, nil
}

// Service owns <root>/kernel, one directory per Tag.
type Service struct {
	root   string
	client *http.Client

	// local names a kernel file to use in place of the release, with the hash it must have.
	local, localSHA256 string
}

// Option configures a Service.
type Option func(*Service)

// WithLocal points every Ensure at one file on disk; a dev path for a stack without a release yet.
func WithLocal(path, sha256 string) Option {
	return func(s *Service) { s.local, s.localSHA256 = path, sha256 }
}

// LocalEnv and LocalSHA256Env name a kernel file and its hash for a dev box without a release; both or neither.
const (
	LocalEnv       = "SHARD_KERNEL"
	LocalSHA256Env = "SHARD_KERNEL_SHA256"
)

// FromEnv reads the dev override, and refuses one that names a file without its hash.
func FromEnv() ([]Option, error) {
	path, sum := os.Getenv(LocalEnv), os.Getenv(LocalSHA256Env)
	if path == "" && sum == "" {
		return nil, nil
	}
	if path == "" || sum == "" {
		return nil, fmt.Errorf("%s and %s must be set together", LocalEnv, LocalSHA256Env)
	}

	return []Option{WithLocal(path, sum)}, nil
}

// WithHTTPClient replaces the client that fetches the release, which a test points at httptest.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.client = c }
}

// New prepares the kernel tree under root, which is the daemon root.
func New(root string, opts ...Option) *Service {
	s := &Service{root: filepath.Join(root, "kernel"), client: http.DefaultClient}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Ensure returns the kernel for arch, downloading it on first use. Every call hashes the file, so a
// kernel that changed on disk never boots.
func (s *Service) Ensure(ctx context.Context, arch string) (Kernel, error) {
	if s.local != "" {
		return verified(s.local, arch, s.localSHA256)
	}

	a, ok := artifacts[arch]
	if !ok {
		return Kernel{}, fmt.Errorf("no guest kernel for arch %q", arch)
	}
	path := filepath.Join(s.root, Tag(), a.name)

	if _, err := os.Stat(path); err == nil {
		return verified(path, arch, a.sha256)
	}

	url, err := URL(arch)
	if err != nil {
		return Kernel{}, err
	}
	if err := s.download(ctx, url, path, a.sha256); err != nil {
		return Kernel{}, fmt.Errorf("download the guest kernel %s: %w", url, err)
	}

	return verified(path, arch, a.sha256)
}

// download fetches url into a sibling part file and renames it only after the hash matched.
func (s *Service) download(ctx context.Context, url, path, want string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}

	part := path + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(part))
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return errors.Join(fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, want), os.Remove(part))
	}

	return os.Rename(part, path)
}

// verified hashes the file at path and returns it as a Kernel only when the hash is want.
func verified(path, arch, want string) (Kernel, error) {
	f, err := os.Open(path)
	if err != nil {
		return Kernel{}, fmt.Errorf("open the guest kernel: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Kernel{}, fmt.Errorf("hash the guest kernel %s: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return Kernel{}, fmt.Errorf("%w for %s: got %s, want %s", ErrChecksum, path, got, want)
	}

	return Kernel{Path: path, Version: Version, Arch: arch, SHA256: got}, nil
}
