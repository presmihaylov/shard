// Package image pulls OCI images and unpacks them into the shared read-only rootfs a sandbox layers over.
package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/presmihaylov/shard/pkg/registry"
)

// ErrNotFound is what a read of an image shard never pulled returns. Match it with errors.Is.
var ErrNotFound = registry.ErrNotCached

// Service owns the image tree: the layout under blobs, and one unpacked rootfs per image.
type Service struct {
	root  string
	store *registry.Store
}

// Image is one pulled image, unpacked and ready for a bundle.
type Image struct {
	Reference string
	Digest    string
	// RootFS is shared and read-only. Every sandbox gets its own writable layer over it.
	RootFS string
	// Size is the download size, not the size on disk after the unpack.
	Size    int64
	Created time.Time
	Config  Config
}

// Config is the part of the image config the bundle builder needs. The entrypoint becomes the supervisor's argv.
type Config struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkDir    string
	User       string
}

// New prepares the image tree under root, which is /var/lib/shard/images on the box.
func New(root string, opts ...registry.Option) (*Service, error) {
	store, err := registry.Open(filepath.Join(root, "layout"), opts...)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join(root, "rootfs"), 0o750); err != nil {
		return nil, fmt.Errorf("create the rootfs directory under %s: %w", root, err)
	}

	return &Service{root: root, store: store}, nil
}

// Pull fetches ref and unpacks it. A second pull of the same reference needs no network.
func (s *Service) Pull(ctx context.Context, ref string) (Image, error) {
	cached, err := s.store.Get(ref)
	if err != nil && !errors.Is(err, registry.ErrNotCached) {
		return Image{}, err
	}

	// A tag we already hold is not re-resolved: shard image rm is how you ask for the newer one.
	if err == nil && s.unpacked(cached) {
		return s.describe(cached)
	}

	pulled, err := s.store.Pull(ctx, ref)
	if err != nil {
		return Image{}, err
	}

	if err := s.unpack(ctx, pulled); err != nil {
		return Image{}, err
	}

	return s.describe(pulled)
}

// List returns every pulled image, ordered by reference.
func (s *Service) List() ([]Image, error) {
	cached, err := s.store.List()
	if err != nil {
		return nil, err
	}

	images := make([]Image, 0, len(cached))
	for _, c := range cached {
		img, err := s.describe(c)
		if err != nil {
			return nil, err
		}

		images = append(images, img)
	}

	return images, nil
}

// Remove deletes the image and its rootfs. SHARD-26 adds the refcount that makes this safe under a sandbox.
func (s *Service) Remove(ref string) error {
	cached, err := s.store.Get(ref)
	if err != nil {
		return err
	}

	if err := s.store.Remove(ref); err != nil {
		return err
	}

	// Another tag can name the same digest, so the rootfs only goes when the last one does.
	shared, err := s.sharesRootFS(cached)
	if err != nil {
		return err
	}

	if shared {
		return nil
	}

	dir := s.rootfsDir(cached)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}

	return nil
}

func (s *Service) sharesRootFS(img registry.Image) (bool, error) {
	others, err := s.store.List()
	if err != nil {
		return false, err
	}

	for _, other := range others {
		if other.Digest == img.Digest {
			return true, nil
		}
	}

	return false, nil
}

func (s *Service) describe(img registry.Image) (Image, error) {
	cfg, err := img.Config()
	if err != nil {
		return Image{}, err
	}

	return Image{
		Reference: img.Reference,
		Digest:    img.Digest,
		RootFS:    s.rootfsDir(img),
		Size:      img.Size,
		Created:   img.Created,
		Config: Config{
			Entrypoint: cfg.Config.Entrypoint,
			Cmd:        cfg.Config.Cmd,
			Env:        cfg.Config.Env,
			WorkDir:    cfg.Config.WorkingDir,
			User:       cfg.Config.User,
		},
	}, nil
}

// rootfsDir is keyed by digest, not by tag, so two tags of one image unpack once.
func (s *Service) rootfsDir(img registry.Image) string {
	return filepath.Join(s.root, "rootfs", strings.ReplaceAll(img.Digest, ":", "-"))
}

func (s *Service) unpacked(img registry.Image) bool {
	info, err := os.Stat(s.rootfsDir(img))

	return err == nil && info.IsDir()
}

// unpack applies the layers into a temporary directory and renames it, so a half unpack never looks done.
func (s *Service) unpack(ctx context.Context, img registry.Image) error {
	if s.unpacked(img) {
		return nil
	}

	layers, err := img.Layers()
	if err != nil {
		return err
	}

	parent := filepath.Join(s.root, "rootfs")

	// The temp directory is a sibling of the final one so that the rename stays on one filesystem.
	tmp, err := os.MkdirTemp(parent, ".unpack-")
	if err != nil {
		return fmt.Errorf("create a temporary directory under %s: %w", parent, err)
	}
	defer os.RemoveAll(tmp)

	// The unpacked directory is the guest root, so a non-root user in the sandbox must be able to traverse it.
	if err := os.Chmod(tmp, 0o755); err != nil { // #nosec G302
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}

	for i, layer := range layers {
		if err := applyLayer(ctx, tmp, layer); err != nil {
			return fmt.Errorf("apply layer %d of %s: %w", i, img.Reference, err)
		}
	}

	if err := os.Rename(tmp, s.rootfsDir(img)); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}

	return nil
}

func applyLayer(ctx context.Context, dir string, layer v1.Layer) (err error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("open the layer: %w", err)
	}
	defer func() {
		// The close is what checks the digest of what we just applied, so it decides the result too.
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close the layer: %w", cerr)
		}
	}()

	_, err = archive.Apply(ctx, dir, rc, applyOpts()...)

	return err
}

// applyOpts keeps the image's own uid and gid, which only root can set. A developer Mac unpacks as itself.
func applyOpts() []archive.ApplyOpt {
	if os.Geteuid() == 0 {
		return nil
	}

	return []archive.ApplyOpt{archive.WithNoSameOwner()}
}
