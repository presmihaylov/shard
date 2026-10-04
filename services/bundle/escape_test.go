package bundle_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// The upper layer is a host directory the guest writes, so a guest symlink there is a host symlink,
// and the root daemon must never follow one out when it writes the layer (SHARD-300).
var escapes = map[string]func(host string) string{
	"absolute": func(host string) string { return host },
	"climbing": func(host string) string { return strings.Repeat("../", 64) + strings.TrimPrefix(host, "/") },
}

// plantLink replaces dir inside the layer with a symlink to target, as a guest running as root can.
func plantLink(t *testing.T, dir, target string) {
	t.Helper()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
}

func TestForkRefusesAGuestSymlinkOutOfTheLayer(t *testing.T) {
	for name, link := range escapes {
		t.Run(name, func(t *testing.T) {
			network := func(id, addr string) models.NetworkSpec {
				return models.NetworkSpec{NetnsPath: "/run/netns/" + id, Address: netip.MustParsePrefix(addr)}
			}
			source := newSpec(t)
			source.Network = network("s-test", "10.87.0.2/16")
			b, _ := build(t, source, models.ImageConfig{})

			host := t.TempDir()
			write(t, filepath.Join(host, "hosts"), "the host's own\n")
			plantLink(t, filepath.Join(b.Upper, "etc"), link(host))

			checkpoint := t.TempDir()
			if err := b.Export(t.Context(), checkpoint); err != nil {
				t.Fatalf("Export: %v", err)
			}

			fork := models.SandboxSpec{ID: "s-fork", StateDir: t.TempDir(), Network: network("s-fork", "10.87.0.3/16")}
			if _, err := newService(t).Fork(checkpoint, fork); err == nil {
				t.Error("Fork followed a guest symlink out of the writable layer")
			}

			if got := readFile(t, filepath.Join(host, "hosts")); got != "the host's own\n" {
				t.Errorf("the fork rewrote a host file through the guest's symlink: %q", got)
			}
			if _, err := os.Stat(filepath.Join(host, "resolv.conf")); !os.IsNotExist(err) {
				t.Errorf("the fork wrote resolv.conf through the guest's symlink: %v", err)
			}
		})
	}
}

func TestTrustProxyRefusesAGuestSymlinkOutOfTheLayer(t *testing.T) {
	for name, link := range escapes {
		t.Run(name, func(t *testing.T) {
			b := built(t)

			host := t.TempDir()
			if err := os.MkdirAll(filepath.Join(host, "certs"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(host, "certs/ca-certificates.crt"), "the host's own\n")
			plantLink(t, filepath.Join(b.Upper, "etc/ssl"), link(host))

			if err := b.TrustProxy([]byte(proxyCA)); err == nil {
				t.Error("TrustProxy followed a guest symlink out of the writable layer")
			}

			if got := readFile(t, filepath.Join(host, "certs/ca-certificates.crt")); got != "the host's own\n" {
				t.Errorf("TrustProxy rewrote a host file through the guest's symlink: %q", got)
			}
		})
	}
}

// A link that stays inside the layer is the guest's own business, and the write lands where the guest reads it.
func TestTrustProxyFollowsAGuestSymlinkInsideTheLayer(t *testing.T) {
	b := built(t)
	if err := os.MkdirAll(filepath.Join(b.Upper, "usr/ssl"), 0o755); err != nil {
		t.Fatal(err)
	}
	plantLink(t, filepath.Join(b.Upper, "etc/ssl"), "../usr/ssl")

	if err := b.TrustProxy([]byte(proxyCA)); err != nil {
		t.Fatalf("TrustProxy: %v", err)
	}

	if got := readFile(t, filepath.Join(b.Upper, "usr/ssl/certs/ca-certificates.crt")); got != imageRoots+proxyCA {
		t.Errorf("the merged bundle is:\n%s", got)
	}
}
