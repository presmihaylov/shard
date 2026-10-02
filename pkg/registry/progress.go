package registry

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Progress hears a pull as it runs. The layout writes the layers in parallel, so Layer must be safe to call concurrently.
type Progress interface {
	// Manifest comes once, before any layer, with the image's name, its digest and the compressed size of each layer.
	Manifest(ref, digest string, layers []int64)
	// Layer comes once per layer as its blob lands; present means the store already held it and nothing was fetched.
	Layer(digest string, size int64, present bool)
}

type quiet struct{}

func (quiet) Manifest(string, string, []int64) {}
func (quiet) Layer(string, int64, bool)        {}

func announce(ref string, img v1.Image, progress Progress) error {
	digest, err := img.Digest()
	if err != nil {
		return fmt.Errorf("digest the manifest: %w", err)
	}

	manifest, err := img.Manifest()
	if err != nil {
		return fmt.Errorf("read the manifest: %w", err)
	}

	sizes := make([]int64, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		sizes[i] = layer.Size
	}
	progress.Manifest(ref, digest.String(), sizes)

	return nil
}

// reportingImage is what WriteImage gets, so each layer it writes reports as its blob lands.
type reportingImage struct {
	v1.Image
	blobs    string
	progress Progress
}

func (i reportingImage) Layers() ([]v1.Layer, error) {
	layers, err := i.Image.Layers()
	if err != nil {
		return nil, err
	}

	wrapped := make([]v1.Layer, len(layers))
	for n, layer := range layers {
		wrapped[n] = reportingLayer{Layer: layer, blobs: i.blobs, progress: i.progress}
	}

	return wrapped, nil
}

type reportingLayer struct {
	v1.Layer
	blobs    string
	progress Progress
}

func (l reportingLayer) Compressed() (io.ReadCloser, error) {
	digest, err := l.Digest()
	if err != nil {
		return nil, err
	}

	size, err := l.Size()
	if err != nil {
		return nil, err
	}

	rc, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}

	return &landing{ReadCloser: rc, digest: digest, size: size, blobs: l.blobs, progress: l.progress}, nil
}

// landing reports a layer once: at EOF with the bytes read, or at a close before any read when the blob is already on disk.
type landing struct {
	io.ReadCloser
	digest   v1.Hash
	size     int64
	blobs    string
	progress Progress
	read     int64
	reported bool
}

func (l *landing) Read(p []byte) (int, error) {
	n, err := l.ReadCloser.Read(p)
	l.read += int64(n)
	if errors.Is(err, io.EOF) && !l.reported {
		l.reported = true
		l.progress.Layer(l.digest.String(), l.read, false)
	}

	return n, err
}

func (l *landing) Close() error {
	// The layout closes unread when it skips a blob it holds, and also when it fails before the copy, so ask the disk which.
	if !l.reported && l.read == 0 && l.held() {
		l.reported = true
		l.progress.Layer(l.digest.String(), l.size, true)
	}

	return l.ReadCloser.Close()
}

func (l *landing) held() bool {
	info, err := os.Stat(filepath.Join(l.blobs, l.digest.Algorithm, l.digest.Hex))

	return err == nil && info.Mode().IsRegular() && info.Size() == l.size
}
