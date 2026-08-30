package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// guestCABundle is where a Linux guest's libraries look for the roots. The image's own list is kept
// and the proxy CA goes after it, so every public host still verifies and so does the proxy.
const guestCABundle = "etc/ssl/certs/ca-certificates.crt"

// proxyCAEnv points the libraries that read no system bundle at the one file.
var proxyCAEnv = []string{
	"SSL_CERT_FILE=/" + guestCABundle,
	"REQUESTS_CA_BUNDLE=/" + guestCABundle,
	"NODE_EXTRA_CA_CERTS=/" + guestCABundle,
}

// writeProxyCA puts the proxy CA into the writable layer, appended to the roots the image ships.
func writeProxyCA(b Bundle, spec models.SandboxSpec) error {
	if len(spec.ProxyCA) == 0 {
		return nil
	}

	roots, err := os.ReadFile(filepath.Join(spec.RootFS, guestCABundle))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read the image roots: %w", err)
	}
	if len(roots) != 0 && roots[len(roots)-1] != '\n' {
		roots = append(roots, '\n')
	}

	path := filepath.Join(b.Upper, guestCABundle)
	if err := os.MkdirAll(filepath.Dir(path), etcDirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := store.WriteFile(path, append(roots, spec.ProxyCA...), etcFilePerm); err != nil { // #nosec G306
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
