package registry_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	ggcr "github.com/google/go-containerregistry/pkg/registry"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/presmihaylov/shard/pkg/registry"
)

func TestPullThenGetOffline(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	store := openStore(t)

	pulled, err := store.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if pulled.Size <= 0 {
		t.Errorf("got size %d, want the sum of the layers", pulled.Size)
	}

	// The registry goes away, so anything that answers now answered from disk.
	server.Close()

	cached, err := store.Get(ref)
	if err != nil {
		t.Fatalf("Get after the registry closed: %v", err)
	}

	if cached.Digest != pulled.Digest {
		t.Errorf("got digest %s, want %s", cached.Digest, pulled.Digest)
	}

	layers, err := cached.Layers()
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}

	if len(layers) != 1 {
		t.Fatalf("got %d layers, want 1", len(layers))
	}

	cfg, err := cached.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}

	if len(cfg.Config.Entrypoint) == 0 {
		t.Error("the cached config lost the entrypoint")
	}
}

func TestPullIsReachableFromASecondStore(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	dir := t.TempDir()

	store, err := registry.Open(dir, registry.WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := store.Pull(t.Context(), ref); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	server.Close()

	// A fresh process reads the same layout, which is the whole point of caching on disk.
	reopened, err := registry.Open(dir)
	if err != nil {
		t.Fatalf("Open the second time: %v", err)
	}

	if _, err := reopened.Get(ref); err != nil {
		t.Fatalf("Get from the reopened store: %v", err)
	}
}

func TestGetMissingImage(t *testing.T) {
	store := openStore(t)

	if _, err := store.Get("app:1.0"); !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v, want ErrNotCached", err)
	}
}

func TestListIsOrderedByReference(t *testing.T) {
	server, first := servedImage(t, "beta:1.0", map[string]string{"/beta": "1"})
	second := pushImage(t, server, "alpha:1.0", map[string]string{"/alpha": "1"})
	store := openStore(t)

	for _, ref := range []string{first, second} {
		if _, err := store.Pull(t.Context(), ref); err != nil {
			t.Fatalf("Pull %s: %v", ref, err)
		}
	}

	images, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}

	if images[0].Reference >= images[1].Reference {
		t.Errorf("List is not ordered: %s came before %s", images[0].Reference, images[1].Reference)
	}
}

func TestPullTwiceKeepsOneEntry(t *testing.T) {
	_, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	store := openStore(t)

	for range 2 {
		if _, err := store.Pull(t.Context(), ref); err != nil {
			t.Fatalf("Pull: %v", err)
		}
	}

	images, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(images) != 1 {
		t.Fatalf("got %d images, want 1", len(images))
	}
}

func TestRemoveDropsTheBlobs(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	dir := t.TempDir()

	store, err := registry.Open(dir, registry.WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := store.Pull(t.Context(), ref); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if err := store.Remove(ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := store.Get(ref); !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v after Remove, want ErrNotCached", err)
	}

	blobs, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(blobs) != 0 {
		t.Errorf("got %d blobs after Remove, want none", len(blobs))
	}
}

func TestRemoveMissingImage(t *testing.T) {
	store := openStore(t)

	if err := store.Remove("app:1.0"); !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v, want ErrNotCached", err)
	}
}

func openStore(t *testing.T) *registry.Store {
	t.Helper()

	store, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return store
}

// servedImage starts an in-process registry and pushes one image into it.
func servedImage(t *testing.T, tag string, files map[string]string) (*httptest.Server, string) {
	t.Helper()

	server := httptest.NewServer(ggcr.New())
	t.Cleanup(server.Close)

	return server, pushImage(t, server, tag, files)
}

func pushImage(t *testing.T, server *httptest.Server, tag string, files map[string]string) string {
	t.Helper()

	host, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse the server URL: %v", err)
	}

	ref, err := name.ParseReference(host.Host + "/shard/" + tag)
	if err != nil {
		t.Fatalf("parse the reference: %v", err)
	}

	img, err := mutate.AppendLayers(empty.Image, tarLayer(t, files))
	if err != nil {
		t.Fatalf("append the layer: %v", err)
	}

	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		OS:           "linux",
		Architecture: "amd64",
		Created:      v1.Time{Time: time.Unix(1700000000, 0).UTC()},
		Config:       v1.Config{Entrypoint: []string{"/bin/sh"}, Env: []string{"PATH=/usr/bin"}},
	})
	if err != nil {
		t.Fatalf("set the config: %v", err)
	}

	if err := remote.Write(ref, img, remote.WithTransport(server.Client().Transport)); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}

	return ref.Name()
}

// tarLayer builds a tar layer. A name that starts with .wh. is a whiteout, exactly as an image would carry it.
func tarLayer(t *testing.T, files map[string]string) v1.Layer {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for path, content := range files {
		header := &tar.Header{Name: path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write the tar header for %s: %v", path, err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatalf("build the layer: %v", err)
	}

	return layer
}
