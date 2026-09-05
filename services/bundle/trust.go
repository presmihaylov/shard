package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// TrustEnv names the variables a fronted sandbox has pointed at its merged CA bundle, so the guest's
// libraries trust the proxy: every TLS client of note reads one of the three.
var TrustEnv = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

// rootPaths is where the common image families keep their CA bundle, relative to the rootfs.
var rootPaths = []string{"etc/ssl/certs/ca-certificates.crt", "etc/pki/tls/certs/ca-bundle.crt", "etc/ssl/ca-bundle.pem"}

// TrustsUser refuses a user environment that points a TLS client away from the merged bundle, because
// the proxy would then fail every request the sandbox was fronted for.
func TrustsUser(env []string) error {
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains(TrustEnv, key) {
			return fmt.Errorf("--env %s cannot be set on a fronted sandbox: the proxy CA is planted there", key)
		}
	}

	return nil
}

// plantTrust writes the image roots plus the proxy CA to the path the guest already reads, and says
// which variables to set so every client reads that path. It never writes the proxy CA alone.
func plantTrust(b Bundle, spec models.SandboxSpec) ([]string, error) {
	rel, roots, err := imageRoots(spec)
	if err != nil {
		return nil, err
	}

	target := filepath.Join(b.Upper, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), etcDirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}

	merged := append(slices.Clone(roots), '\n')
	if roots[len(roots)-1] == '\n' {
		merged = slices.Clone(roots)
	}
	merged = append(merged, spec.ProxyCA...)

	if err := store.WriteFile(target, merged, etcFilePerm); err != nil { // #nosec G306
		return nil, fmt.Errorf("write %s: %w", target, err)
	}

	env := make([]string, 0, len(TrustEnv))
	for _, key := range TrustEnv {
		env = append(env, key+"=/"+rel)
	}

	return env, nil
}

// imageRoots reads the image's own CA bundle from the first path that holds one, inside the rootfs only.
func imageRoots(spec models.SandboxSpec) (string, []byte, error) {
	root, err := os.OpenRoot(spec.RootFS)
	if err != nil {
		return "", nil, fmt.Errorf("open the image rootfs %s: %w", spec.RootFS, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	candidates := slices.Clone(rootPaths)
	if named := envValue(spec.Env, "SSL_CERT_FILE"); named != "" {
		candidates = append([]string{strings.TrimPrefix(path.Clean("/"+named), "/")}, candidates...)
	}

	for _, rel := range candidates {
		roots, err := root.ReadFile(rel)
		if errors.Is(err, fs.ErrNotExist) || len(roots) == 0 && err == nil {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("read the CA bundle %s of the image rootfs %s: %w", rel, spec.RootFS, err)
		}

		return rel, roots, nil
	}

	return "", nil, fmt.Errorf("the image rootfs %s has no CA bundle to add the proxy CA to; tried /%s", spec.RootFS, strings.Join(candidates, ", /"))
}

func envValue(env []string, name string) string {
	for _, entry := range slices.Backward(env) {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name {
			return value
		}
	}

	return ""
}
