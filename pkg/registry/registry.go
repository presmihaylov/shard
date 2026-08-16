// Package registry pulls OCI images and keeps them in an OCI image layout on disk.
package registry

import (
	"bytes"
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

// ErrNotReclaimed marks a removal that finished but could not free the blobs behind it.
var ErrNotReclaimed = errors.New("the image was removed but its blobs were not reclaimed")

// responseHeaderTimeout bounds a registry that accepts the connection and then says nothing.
const responseHeaderTimeout = 30 * time.Second

// Store is a local OCI image layout. Every write lands atomically, so a killed pull leaves no half blob.
type Store struct {
	path      layout.Path
	platform  v1.Platform
	keychain  authn.Keychain
	transport http.RoundTripper
	insecure  map[string]bool
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

// WithInsecureRegistries allows plaintext http to these hosts. Every other host is https or nothing.
func WithInsecureRegistries(hosts ...string) Option {
	return func(s *Store) {
		for _, host := range hosts {
			s.insecure[host] = true
		}
	}
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
		transport: defaultTransport(),
		insecure:  map[string]bool{},
	}
	for _, opt := range opts {
		opt(s)
	}

	// Last, so it wraps whatever transport the options settled on and no caller can opt out of it.
	s.transport = httpsOnly{next: s.transport, allowed: s.insecure}

	return s, nil
}

// remote.DefaultTransport sets a dial and a handshake timeout and nothing else, so a mute registry hangs.
func defaultTransport() http.RoundTripper {
	base, ok := remote.DefaultTransport.(*http.Transport)
	if !ok {
		return remote.DefaultTransport
	}

	rt := base.Clone()
	rt.ResponseHeaderTimeout = responseHeaderTimeout

	return rt
}

// httpsOnly refuses plaintext, because ggcr silently downgrades every RFC1918 and loopback registry.
type httpsOnly struct {
	next    http.RoundTripper
	allowed map[string]bool
}

func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" || t.allowed[req.URL.Host] {
		return t.next.RoundTrip(req) //nolint:wrapcheck // a RoundTripper returns the transport's error as it is
	}

	return nil, fmt.Errorf("refusing plaintext http to %s: pass --insecure-registry %s to allow it", req.URL.Host, req.URL.Host)
}

// Image is one cached image. It carries no layer bytes; ask Layers for those.
type Image struct {
	// Reference is canonical, so alpine:3.20 reads back as index.docker.io/library/alpine:3.20.
	Reference string
	Digest    string
	// Size is the download size, the sum of the compressed layers.
	Size    int64
	Created time.Time
	// Broken is set when the index still names the image but its blobs do not read back.
	Broken error

	img v1.Image
}

// Entry is one index.json record. Remove and the rootfs refcount work off these, so neither reads a blob.
type Entry struct {
	Reference string
	Digest    string
}

// Config is the image config: entrypoint, env and the rest of what the bundle builder needs.
func (i Image) Config() (*v1.ConfigFile, error) {
	if i.Broken != nil {
		return nil, i.Broken
	}

	cfg, err := i.img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read the config of %s: %w", i.Reference, err)
	}

	return cfg, nil
}

// Layers returns the layers bottom up, which is the order they must be applied in.
func (i Image) Layers() ([]v1.Layer, error) {
	if i.Broken != nil {
		return nil, i.Broken
	}

	layers, err := i.img.Layers()
	if err != nil {
		return nil, fmt.Errorf("read the layers of %s: %w", i.Reference, err)
	}

	return layers, nil
}

// Pull fetches ref and caches it. A caller that cannot use what it got rolls the entry back with Remove.
func (s *Store) Pull(ctx context.Context, ref string) (Image, error) {
	parsed, err := parseRef(ref)
	if err != nil {
		return Image{}, err
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

	// Read back off the layout rather than off the wire, so a pull that reports success is a pull you can use.
	return s.Get(parsed.Name())
}

// Get returns a cached image and never touches the network.
func (s *Store) Get(ref string) (Image, error) {
	matched, err := s.match(ref)
	if err != nil {
		return Image{}, err
	}

	return s.image(matched[0])
}

// Match returns every index entry ref names, by tag or by digest. It reads index.json and no blob.
func (s *Store) Match(ref string) ([]Entry, error) {
	matched, err := s.match(ref)
	if err != nil {
		return nil, err
	}

	return entriesOf(matched), nil
}

func (s *Store) match(ref string) ([]v1.Descriptor, error) {
	parsed, err := parseRef(ref)
	if err != nil {
		return nil, err
	}

	manifests, err := s.manifests()
	if err != nil {
		return nil, err
	}

	matched := slices.DeleteFunc(slices.Clone(manifests), func(d v1.Descriptor) bool { return !names(d, parsed) })
	if len(matched) == 0 {
		return nil, s.notCached(parsed, manifests)
	}

	return matched, nil
}

// Entries returns every index.json record, without reading a single blob.
func (s *Store) Entries() ([]Entry, error) {
	manifests, err := s.manifests()
	if err != nil {
		return nil, err
	}

	return entriesOf(manifests), nil
}

func entriesOf(manifests []v1.Descriptor) []Entry {
	entries := make([]Entry, 0, len(manifests))
	for _, desc := range manifests {
		entries = append(entries, Entry{Reference: desc.Annotations[refAnnotation], Digest: desc.Digest.String()})
	}

	return entries
}

// names matches the tag annotation, and the descriptor digest too, so image ls output feeds image rm.
func names(d v1.Descriptor, parsed name.Reference) bool {
	if d.Annotations[refAnnotation] == parsed.Name() {
		return true
	}

	digest, ok := parsed.(name.Digest)

	return ok && d.Digest.String() == digest.DigestStr()
}

// notCached names the tags the store does hold for that repository, so a near miss is obvious.
func (s *Store) notCached(parsed name.Reference, manifests []v1.Descriptor) error {
	repository := parsed.Context().Name()

	var held []string
	for _, desc := range manifests {
		ref := desc.Annotations[refAnnotation]
		if strings.HasPrefix(ref, repository+":") || strings.HasPrefix(ref, repository+"@") {
			held = append(held, ref)
		}
	}

	if len(held) == 0 {
		return fmt.Errorf("%s: %w", parsed.Name(), ErrNotCached)
	}

	slices.Sort(held)

	return fmt.Errorf("%s: %w; the store holds %s", parsed.Name(), ErrNotCached, strings.Join(held, ", "))
}

// List returns every cached image, ordered by reference. One unreadable entry no longer hides the rest.
func (s *Store) List() ([]Image, error) {
	manifests, err := s.manifests()
	if err != nil {
		return nil, err
	}

	images := make([]Image, 0, len(manifests))
	for _, desc := range manifests {
		img, err := s.image(desc)
		if err != nil {
			img = Image{Reference: desc.Annotations[refAnnotation], Digest: desc.Digest.String(), Broken: err}
		}

		images = append(images, img)
	}

	slices.SortFunc(images, func(a, b Image) int { return strings.Compare(a.Reference, b.Reference) })

	return images, nil
}

// Remove drops ref from the index and deletes every blob no remaining image references.
func (s *Store) Remove(ref string) error {
	matched, err := s.match(ref)
	if err != nil {
		return err
	}

	manifests, err := s.manifests()
	if err != nil {
		return err
	}

	dropped := entriesOf(matched)
	kept := slices.DeleteFunc(slices.Clone(manifests), func(d v1.Descriptor) bool {
		return slices.Contains(dropped, Entry{Reference: d.Annotations[refAnnotation], Digest: d.Digest.String()})
	})

	if err := s.writeIndex(kept); err != nil {
		return err
	}

	// The removal is done at this point, so a collect failure costs disk space and never correctness.
	if err := s.Collect(); err != nil {
		return fmt.Errorf("%w: %w", ErrNotReclaimed, err)
	}

	return nil
}

// Collect deletes every blob the index no longer reaches. A shared blob stays as long as one image needs it.
func (s *Store) Collect() error {
	reachable, err := s.reachable()
	if err != nil {
		return err
	}

	root := filepath.Join(string(s.path), "blobs")
	algorithms, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", root, err)
	}

	for _, algorithm := range algorithms {
		if !algorithm.IsDir() {
			continue
		}

		if err := s.collectAlgorithm(algorithm.Name(), reachable); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) collectAlgorithm(algorithm string, reachable map[v1.Hash]bool) error {
	dir := filepath.Join(string(s.path), "blobs", algorithm)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// A name that is not a digest is debris from a killed pull. ggcr's own collector chokes on it forever.
		hash, err := v1.NewHash(algorithm + ":" + entry.Name())
		if err == nil && reachable[hash] {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	return nil
}

// reachable is every blob index.json still points at: each manifest, its config and its layers.
func (s *Store) reachable() (map[v1.Hash]bool, error) {
	manifests, err := s.manifests()
	if err != nil {
		return nil, err
	}

	reachable := map[v1.Hash]bool{}
	for _, desc := range manifests {
		reachable[desc.Digest] = true

		raw, err := s.path.Bytes(desc.Digest)
		if err != nil {
			// An entry whose own manifest is gone is already broken, and holding its layers would wedge every rm.
			continue
		}

		manifest, err := v1.ParseManifest(bytes.NewReader(raw))
		if err != nil {
			continue
		}

		reachable[manifest.Config.Digest] = true
		for _, layer := range manifest.Layers {
			reachable[layer.Digest] = true
		}
	}

	return reachable, nil
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

	desc, err := partial.Descriptor(img)
	if err != nil {
		return fmt.Errorf("describe %s: %w", ref, err)
	}
	desc.Annotations = map[string]string{refAnnotation: ref}

	manifests, err := s.manifests()
	if err != nil {
		return err
	}

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

// parseRef also rejects what ParseReference accepts and a later path join would not: a . or .. segment.
func parseRef(ref string) (name.Reference, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse the reference %q: %w", ref, err)
	}

	for segment := range strings.SplitSeq(parsed.Context().RepositoryStr(), "/") {
		if segment == "." || segment == ".." {
			return nil, fmt.Errorf("the reference %q has a %q path segment", ref, segment)
		}
	}

	return parsed, nil
}
