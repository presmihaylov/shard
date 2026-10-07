package bundle

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/services/runspec"
)

// TrustEnv names the variables a fronted sandbox has pointed at its merged CA bundle, so the guest's
// libraries trust the proxy; curl reads CURL_CA_BUNDLE first, and the official curl image sets its own.
var TrustEnv = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "CURL_CA_BUNDLE"}

// ErrNoCABundle is an image tree with none of the roots the proxy CA joins, so no fronted sandbox can run on it.
var ErrNoCABundle = errors.New("no CA bundle")

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

// Store is the trust store of a fronted sandbox: the merged roots, the guest path the image reads them from, and the variables that point there.
type Store struct {
	Path  string
	Roots []byte
	Env   []string
}

// Roots is the CA bundle a tree holds before the proxy CA joins it, and its path there; no path is a tree with none.
type Roots struct {
	Path string `json:"path,omitempty"`
	PEM  []byte `json:"pem,omitempty"`
}

// ReadRoots reads the tree's own CA bundle; a tree with none answers empty roots, which only a fronted sandbox refuses.
func ReadRoots(rootfs string, env []string) (Roots, error) {
	rel, roots, err := imageRoots(rootfs, env)
	if errors.Is(err, ErrNoCABundle) {
		return Roots{}, nil
	}
	if err != nil {
		return Roots{}, err
	}

	return Roots{Path: "/" + rel, PEM: roots}, nil
}

// Trust merges the proxy CA into the image's own roots; a substrate with no upper layer hands it to the guest to write.
func Trust(rootfs string, env []string, proxyCA []byte) (Store, error) {
	return trustIn(rootfs, "the image rootfs "+rootfs, env, proxyCA)
}

// trustIn merges the proxy CA into the roots of tree, which a refusal calls name.
func trustIn(tree, name string, env []string, proxyCA []byte) (Store, error) {
	rel, roots, err := imageRoots(tree, env)
	if err != nil {
		return Store{}, fmt.Errorf("%s: %w", name, err)
	}

	return Roots{Path: "/" + rel, PEM: roots}.Trust(proxyCA)
}

// Trust adds the proxy CA to the roots once: a tree a fronted sandbox left, as a snapshot keeps it, holds the CA already.
func (r Roots) Trust(proxyCA []byte) (Store, error) {
	if len(r.PEM) == 0 {
		return Store{}, fmt.Errorf("the sandbox's tree has %w to add the proxy CA to", ErrNoCABundle)
	}

	merged := slices.Clone(r.PEM)
	if !bytes.Contains(r.PEM, bytes.TrimSpace(proxyCA)) {
		if merged[len(merged)-1] != '\n' {
			merged = append(merged, '\n')
		}
		merged = append(merged, proxyCA...)
	}

	trust := make([]string, 0, len(TrustEnv))
	for _, key := range TrustEnv {
		trust = append(trust, key+"="+r.Path)
	}

	return Store{Path: r.Path, Roots: merged, Env: trust}, nil
}

// plantTrust writes the merged store into layer, and says which variables point every client at it.
func plantTrust(layer string, trust Store, shift idShift) ([]string, error) {
	if err := writeLayer(layer, filepath.FromSlash(strings.TrimPrefix(trust.Path, "/")), trust.Roots, shift); err != nil {
		return nil, err
	}

	return trust.Env, nil
}

// imageRoots reads the tree's own CA bundle from the first path that holds one, inside the tree only.
func imageRoots(rootfs string, env []string) (string, []byte, error) {
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return "", nil, fmt.Errorf("open %s: %w", rootfs, err)
	}
	defer root.Close() //nolint:errcheck // a read-only handle has nothing left to flush

	candidates := rootCandidates(env)
	for _, rel := range candidates {
		roots, err := root.ReadFile(rel)
		if errors.Is(err, fs.ErrNotExist) || len(roots) == 0 && err == nil {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("read the CA bundle /%s: %w", rel, err)
		}

		return rel, roots, nil
	}

	return "", nil, fmt.Errorf("%w to add the proxy CA to; tried /%s", ErrNoCABundle, strings.Join(candidates, ", /"))
}

// rootCandidates is where a tree may keep its CA bundle, the one SSL_CERT_FILE names first.
func rootCandidates(env []string) []string {
	candidates := slices.Clone(rootPaths)
	if named := envValue(env, "SSL_CERT_FILE"); named != "" {
		candidates = append([]string{strings.TrimPrefix(path.Clean("/"+named), "/")}, candidates...)
	}

	return candidates
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

// TrustProxy plants the proxy CA in a built bundle, over the roots the sandbox holds now, once.
func (b Bundle) TrustProxy(proxyCA []byte) error {
	if len(proxyCA) == 0 {
		return errors.New("no proxy CA: there is nothing for the sandbox to trust")
	}

	spec, err := b.readSpec()
	if err != nil {
		return err
	}

	rootfs := spec.Annotations[rootfsAnnotation]
	if rootfs == "" {
		return fmt.Errorf("%s names no image rootfs, so the image roots cannot be read", b.configPath())
	}

	var trust []string
	err = b.withDisk(func() error {
		layer, err := b.lateLayer()
		if err != nil {
			return err
		}
		shift, err := b.layerShift()
		if err != nil {
			return err
		}
		// A mounted overlay shows the merged view itself; a bare upper layer shows it only over the image (SHARD-784).
		layers := []string{layer}
		if layer == b.Upper {
			layers = append(layers, rootfs)
		}
		store, err := layeredTrust(layers, "the sandbox's files over the image rootfs "+rootfs, spec.Process.Env, proxyCA)
		if err != nil {
			return err
		}
		trust, err = plantTrust(layer, store, shift)

		return err
	})
	if err != nil {
		return err
	}

	// shard owns the trust store of a fronted sandbox, so a variable the image set is pointed at the merged bundle.
	spec.Process.Env = runspec.MergeEnv(spec.Process.Env, trust)

	return b.writeSpec(spec)
}

// layeredTrust merges the proxy CA into the roots the overlay of layers shows, which a refusal calls name.
func layeredTrust(layers []string, name string, env []string, proxyCA []byte) (Store, error) {
	tree, err := layeredTree(layers, rootCandidates(env))
	if err != nil {
		return Store{}, err
	}
	trust, err := trustIn(tree, name, env, proxyCA)

	return trust, errors.Join(err, os.RemoveAll(tree))
}
