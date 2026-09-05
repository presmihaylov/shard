package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// guestCABundle is where a Linux guest's libraries look for the roots. The image's own list is kept
// and the proxy CA goes after it, so every public host still verifies and so does the proxy.
const guestCABundle = "etc/ssl/certs/ca-certificates.crt"

// imageCABundles is where each distribution family ships its roots: Debian and Alpine, RHEL, then SUSE.
var imageCABundles = []string{guestCABundle, "etc/pki/tls/certs/ca-bundle.crt", "etc/ssl/ca-bundle.pem"}

// proxyCAEnv points the libraries that read no system bundle at the one file.
var proxyCAEnv = []string{
	"SSL_CERT_FILE=/" + guestCABundle,
	"REQUESTS_CA_BUNDLE=/" + guestCABundle,
	"NODE_EXTRA_CA_CERTS=/" + guestCABundle,
}

// writeProxyCA puts the proxy CA into the writable layer, appended to the roots the image ships.
func writeProxyCA(b Bundle, spec models.SandboxSpec) error {
	return plantProxyCA(b, spec.RootFS, spec.Env, spec.ProxyCA)
}

func plantProxyCA(b Bundle, rootFS string, env []string, ca []byte) error {
	if len(ca) == 0 {
		return nil
	}

	// An image may link that path at a host file, and a secret is one, so the read cannot leave the rootfs.
	rootfs, err := os.OpenRoot(rootFS)
	if err != nil {
		return fmt.Errorf("open the rootfs: %w", err)
	}
	defer rootfs.Close()

	roots, err := imageRoots(rootfs.FS(), rootFS, env)
	if err != nil {
		return err
	}
	if roots[len(roots)-1] != '\n' {
		roots = append(roots, '\n')
	}

	path := filepath.Join(b.Upper, guestCABundle)
	if err := os.MkdirAll(filepath.Dir(path), etcDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := store.WriteFile(path, append(roots, ca...), etcFilePerm); err != nil { // #nosec G306
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// imageRoots is the first bundle the image ships; the env replaces the system roots with the written file, so none means a refusal.
func imageRoots(rootfs fs.FS, rootFS string, env []string) ([]byte, error) {
	candidates := slices.Clone(imageCABundles)
	if own := envValue(env, "SSL_CERT_FILE"); own != "" {
		candidates = slices.Insert(candidates, 0, strings.TrimPrefix(own, "/"))
	}

	for _, candidate := range candidates {
		roots, err := fs.ReadFile(rootfs, candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the image roots: %w", err)
		}
		if len(roots) == 0 {
			continue
		}

		return roots, nil
	}

	return nil, fmt.Errorf("the image at %s ships no CA bundle at /%s, so the guest would trust the proxy alone", rootFS, strings.Join(candidates, ", /"))
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		if key, value, found := strings.Cut(entry, "="); found && key == name {
			return value
		}
	}

	return ""
}
