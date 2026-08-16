// Package registry pulls OCI images and keeps them in an OCI image layout on disk.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/presmihaylov/shard/pkg/store"
)

// refAnnotation is how the OCI layout spec names a manifest, so index.json stays our only ref index.
const refAnnotation = "org.opencontainers.image.ref.name"

// ErrNotCached is what every read of an absent image returns. Match it with errors.Is.
var ErrNotCached = errors.New("image not in the local store")

// Store is a local OCI image layout. Every write lands atomically, so a killed pull leaves no half blob.
type Store struct {
	path      layout.Path
	platform  v1.Platform
	keychain  authn.Keychain
	transport http.RoundTripper
}

type Option func(*Store)

// WithPlatform picks the image out of a manifest list. The box is x86_64 and so is every benchmark.
func WithPlatform(p v1.Platform) Option {
	return func(s *Store) { s.platform = p }
}

// WithTransport replaces the HTTP transport, which is how the tests reach an in-process registry.
func WithTransport(rt http.RoundTripper) Option {
	return func(s *Store) { s.transport = rt }
}

// Open reads the layout at dir, and creates an empty one if there is none.
func Open(dir string, opts ...Option) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	path, err := layout.FromPath(dir)
	if err != nil {
		path, err = layout.Write(dir, empty.Index)
		if err != nil {
			return nil, fmt.Errorf("initialise the layout at %s: %w", dir, err)
		}
	}

	s := &Store{
		path:      path,
		platform:  v1.Platform{OS: "linux", Architecture: "amd64"},
		keychain:  authn.DefaultKeychain,
		transport: remote.DefaultTransport,
	}
	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

// Image is one cached image. It carries no layer bytes; ask Layers for those.
type Image struct {
	// Reference is canonical, so alpine:3.20 reads back as index.docker.io/library/alpine:3.20.
	Reference string
	Digest    string
	// Size is the download size, the sum of the compressed layers.
	Size    int64
	Created time.Time

	img v1.Image
}

// Config is the image config: entrypoint, env and the rest of what the bundle builder needs.
func (i Image) Config() (*v1.ConfigFile, error) {
	cfg, err := i.img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read the config of %s: %w", i.Reference, err)
	}

	return cfg, nil
}

// Layers returns the layers bottom up, which is the order they must be applied in.
func (i Image) Layers() ([]v1.Layer, error) {
	layers, err := i.img.Layers()
	if err != nil {
		return nil, fmt.Errorf("read the layers of %s: %w", i.Reference, err)
	}

	return layers, nil
}

// Pull fetches ref, caches it, and returns what it reads back from the cache.
func (s *Store) Pull(ctx context.Context, ref string) (Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Image{}, fmt.Errorf("parse the reference %q: %w", ref, err)
	}

	img, err := remote.Image(parsed,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(s.keychain),
		remote.WithTransport(s.transport),
		remote.WithPlatform(s.platform),
	)
	if err != nil {
		return Image{}, fmt.Errorf("fetch %s: %w", parsed.Name(), err)
	}

	if err := s.write(parsed.Name(), img); err != nil {
		return Image{}, err
	}

	// Read back rather than return what came off the wire, so a pull that reports success is a pull you can use.
	return s.Get(ref)
}

// Get returns a cached image and never touches the network.
func (s *Store) Get(ref string) (Image, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return Image{}, fmt.Errorf("parse the reference %q: %w", ref, err)
	}

	manifests, err := s.manifests()
	if err != nil {
		return Image{}, err
	}

	for _, desc := range manifests {
		if desc.Annotations[refAnnotation] == parsed.Name() {
			return s.image(desc)
		}
	}

	return Image{}, fmt.Errorf("%s: %w", parsed.Name(), ErrNotCached)
}

// List returns every cached image, ordered by reference.
func (s *Store) List() ([]Image, error) {
	manifests, err := s.manifests()
	if err != nil {
		return nil, err
	}

	images := make([]Image, 0, len(manifests))
	for _, desc := range manifests {
		img, err := s.image(desc)
		if err != nil {
			return nil, err
		}

		images = append(images, img)
	}

	slices.SortFunc(images, func(a, b Image) int { return strings.Compare(a.Reference, b.Reference) })

	return images, nil
}

// Remove drops ref from the index and deletes every blob no remaining image references.
func (s *Store) Remove(ref string) error {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("parse the reference %q: %w", ref, err)
	}

	manifests, err := s.manifests()
	if err != nil {
		return err
	}

	kept := slices.DeleteFunc(slices.Clone(manifests), func(d v1.Descriptor) bool {
		return d.Annotations[refAnnotation] == parsed.Name()
	})
	if len(kept) == len(manifests) {
		return fmt.Errorf("%s: %w", parsed.Name(), ErrNotCached)
	}

	if err := s.writeIndex(kept); err != nil {
		return err
	}

	return s.collect()
}

// collect deletes the blobs the index no longer reaches. A shared blob stays as long as one image needs it.
func (s *Store) collect() error {
	unreferenced, err := s.path.GarbageCollect()
	if err != nil {
		return fmt.Errorf("find the unreferenced blobs: %w", err)
	}

	for _, hash := range unreferenced {
		if err := s.path.RemoveBlob(hash); err != nil {
			return fmt.Errorf("remove the blob %s: %w", hash, err)
		}
	}

	return nil
}

func (s *Store) write(ref string, img v1.Image) error {
	// The layout writes the config and the manifest with a bare os.Create, so land those two ourselves first.
	if err := s.writeJSONBlob(img.ConfigName, img.RawConfigFile); err != nil {
		return err
	}

	if err := s.writeJSONBlob(img.Digest, img.RawManifest); err != nil {
		return err
	}

	// The layers it does write through a temp file and a rename, and it skips a blob it already has.
	if err := s.path.WriteImage(img); err != nil {
		return fmt.Errorf("write the layers of %s: %w", ref, err)
	}

	manifests, err := s.manifests()
	if err != nil {
		return err
	}

	desc, err := partial.Descriptor(img)
	if err != nil {
		return fmt.Errorf("describe %s: %w", ref, err)
	}
	desc.Annotations = map[string]string{refAnnotation: ref}

	kept := slices.DeleteFunc(slices.Clone(manifests), func(d v1.Descriptor) bool {
		return d.Annotations[refAnnotation] == ref
	})

	return s.writeIndex(append(kept, *desc))
}

func (s *Store) writeJSONBlob(hash func() (v1.Hash, error), raw func() ([]byte, error)) error {
	h, err := hash()
	if err != nil {
		return fmt.Errorf("digest a blob: %w", err)
	}

	data, err := raw()
	if err != nil {
		return fmt.Errorf("read the blob %s: %w", h, err)
	}

	dir := filepath.Join(string(s.path), "blobs", h.Algorithm)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	return store.WriteFile(filepath.Join(dir, h.Hex), data, 0o644)
}

func (s *Store) manifests() ([]v1.Descriptor, error) {
	index, err := s.path.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("read the layout index: %w", err)
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("parse the layout index: %w", err)
	}

	return manifest.Manifests, nil
}

func (s *Store) writeIndex(manifests []v1.Descriptor) error {
	index, err := s.path.ImageIndex()
	if err != nil {
		return fmt.Errorf("read the layout index: %w", err)
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		return fmt.Errorf("parse the layout index: %w", err)
	}
	manifest.Manifests = manifests

	raw, err := json.MarshalIndent(manifest, "", "   ")
	if err != nil {
		return fmt.Errorf("encode the layout index: %w", err)
	}

	return store.WriteFile(filepath.Join(string(s.path), "index.json"), raw, 0o644)
}

func (s *Store) image(desc v1.Descriptor) (Image, error) {
	ref := desc.Annotations[refAnnotation]

	img, err := s.path.Image(desc.Digest)
	if err != nil {
		return Image{}, fmt.Errorf("read %s from the store: %w", ref, err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return Image{}, fmt.Errorf("parse the manifest of %s: %w", ref, err)
	}

	var size int64
	for _, layer := range manifest.Layers {
		size += layer.Size
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return Image{}, fmt.Errorf("parse the config of %s: %w", ref, err)
	}

	return Image{
		Reference: ref,
		Digest:    desc.Digest.String(),
		Size:      size,
		Created:   cfg.Created.Time,
		img:       img,
	}, nil
}
