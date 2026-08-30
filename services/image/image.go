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

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/registry"
	"github.com/presmihaylov/shard/pkg/store"
)

// ErrNotFound is what a read of an image shard never pulled returns. Match it with errors.Is.
var ErrNotFound = registry.ErrNotCached

// ErrNotReclaimed marks a removal that finished but could not free the blobs behind it.
var ErrNotReclaimed = registry.ErrNotReclaimed

// stagingPrefix names the tree an unpack builds before it renames it into place under the digest.
const stagingPrefix = ".unpack-"

// lockFile serializes the writers of one image tree. reclaim sweeps the whole store by reachability,
// so without it one pull's rollback deletes the blobs another pull has written but not yet indexed.
const lockFile = ".pull.lock"

const lockPerm = 0o600

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
	Config  models.ImageConfig
	// Broken is set when the index still names the image but its blobs do not read back.
	Broken error
}

// New prepares the image tree under root, which is /var/lib/shard/images on the box.
func New(root string, opts ...registry.Option) (*Service, error) {
	store, err := registry.Open(filepath.Join(root, "layout"), opts...)
	if err != nil {
		return nil, err
	}

	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfs, 0o750); err != nil {
		return nil, fmt.Errorf("create the rootfs directory under %s: %w", root, err)
	}

	return &Service{root: root, store: store}, nil
}

// Pull fetches ref and unpacks it. A second pull of the same reference needs no network.
func (s *Service) Pull(ctx context.Context, ref string) (_ Image, err error) {
	// The cache is read before the lock, so a pulled image still runs while another pull downloads.
	img, found, err := s.cached(ref)
	if err != nil || found {
		return img, err
	}

	l, err := store.AcquireContext(ctx, filepath.Join(s.root, lockFile), lockPerm)
	if err != nil {
		return Image{}, err
	}
	defer func() { err = errors.Join(err, l.Release()) }()

	// Whoever held the lock may have been pulling this very reference.
	img, found, err = s.cached(ref)
	if err != nil || found {
		return img, err
	}

	if err := s.sweepStaging(); err != nil {
		return Image{}, err
	}

	pulled, err := s.store.Pull(ctx, ref)
	if err != nil {
		return Image{}, errors.Join(err, s.reclaim())
	}

	// index.json is the record of what the store holds, so an image with no rootfs must not stay in it.
	if err := s.unpack(ctx, pulled); err != nil {
		return Image{}, errors.Join(err, s.store.Remove(ref))
	}

	return s.describe(pulled)
}

// cached answers with the image the store already holds unpacked. A tag we hold is not re-resolved:
// shard image rm is how you ask for the newer one.
func (s *Service) cached(ref string) (Image, bool, error) {
	held, err := s.store.Get(ref)
	if errors.Is(err, registry.ErrNotCached) {
		return Image{}, false, nil
	}
	if err != nil {
		return Image{}, false, err
	}

	if !s.unpacked(held) {
		return Image{}, false, nil
	}

	img, err := s.describe(held)

	return img, err == nil, err
}

// reclaim drops the blobs a failed pull left behind, which nothing else reaches once the index misses them.
func (s *Service) reclaim() error {
	if err := s.store.Collect(); err != nil {
		return fmt.Errorf("reclaim the blobs of the failed pull: %w", err)
	}

	return nil
}

// Canonical names the image the way a sandbox record does, so a reference an operator typed compares to it.
func Canonical(ref string) (string, error) {
	return registry.Canonical(ref)
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

// Remove deletes the image and its rootfs. The caller checks no sandbox references it: the records are not its.
func (s *Service) Remove(ctx context.Context, ref string) (err error) {
	// The removal reclaims by reachability too, so it waits for a pull the same way a pull waits.
	l, err := store.AcquireContext(ctx, filepath.Join(s.root, lockFile), lockPerm)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, l.Release()) }()

	// A host whose images are all cached never reaches the sweep in Pull, so the reclaim verb runs it.
	if err := s.sweepStaging(); err != nil {
		return err
	}

	// Orphaned reads index.json only, so an image whose blobs are damaged is still removable.
	orphaned, err := s.store.Orphaned(ref)
	if err != nil {
		return err
	}

	// The rootfs goes first: index.json is the record of what the store holds, so it changes last.
	for _, digest := range orphaned {
		dir := s.rootfsDir(digest)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}

	return s.store.Remove(ref)
}

func (s *Service) describe(img registry.Image) (Image, error) {
	described := Image{
		Reference: img.Reference,
		Digest:    img.Digest,
		RootFS:    s.rootfsDir(img.Digest),
		Size:      img.Size,
		Created:   img.Created,
		Broken:    img.Broken,
	}
	if img.Broken != nil {
		return described, nil
	}

	cfg, err := img.Config()
	if err != nil {
		return Image{}, err
	}

	described.Config = models.ImageConfig{
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
		Env:        cfg.Config.Env,
		WorkDir:    cfg.Config.WorkingDir,
		User:       cfg.Config.User,
	}

	return described, nil
}

// rootfsDir is keyed by digest, not by tag, so two tags of one image unpack once.
func (s *Service) rootfsDir(digest string) string {
	return filepath.Join(s.root, "rootfs", strings.ReplaceAll(digest, ":", "-"))
}

func (s *Service) unpacked(img registry.Image) bool {
	info, err := os.Stat(s.rootfsDir(img.Digest))

	return err == nil && info.IsDir()
}

// sweepStaging drops the tree a killed pull left mid-unpack. It runs under the lock and never in
// New, because a staging tree another writer holds is a live unpack rather than debris.
func (s *Service) sweepStaging() error {
	rootfs := filepath.Join(s.root, "rootfs")

	entries, err := os.ReadDir(rootfs)
	if err != nil {
		return fmt.Errorf("read %s: %w", rootfs, err)
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), stagingPrefix) {
			continue
		}

		path := filepath.Join(rootfs, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove the stale staging tree %s: %w", path, err)
		}
	}

	return nil
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
	tmp, err := os.MkdirTemp(parent, stagingPrefix)
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

	if err := os.Rename(tmp, s.rootfsDir(img.Digest)); err != nil {
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
