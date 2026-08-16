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
	"strings"
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
	store := openStore(t, server)

	pulled := pull(t, store, ref)

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
	store := openStoreAt(t, dir, server)

	pull(t, store, ref)

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
	store := openStore(t, nil)

	if _, err := store.Get("app:1.0"); !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v, want ErrNotCached", err)
	}
}

func TestListIsOrderedByReference(t *testing.T) {
	server, first := servedImage(t, "beta:1.0", map[string]string{"/beta": "1"})
	second := pushImage(t, server, "alpha:1.0", map[string]string{"/alpha": "1"})
	store := openStore(t, server)

	for _, ref := range []string{first, second} {
		pull(t, store, ref)
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
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	store := openStore(t, server)

	for range 2 {
		pull(t, store, ref)
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
	store := openStoreAt(t, dir, server)

	pull(t, store, ref)

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
	store := openStore(t, nil)

	if err := store.Remove("app:1.0"); !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v, want ErrNotCached", err)
	}
}

func openStore(t *testing.T, server *httptest.Server) *registry.Store {
	t.Helper()

	return openStoreAt(t, t.TempDir(), server)
}

func openStoreAt(t *testing.T, dir string, server *httptest.Server) *registry.Store {
	t.Helper()

	var opts []registry.Option
	if server != nil {
		opts = append(opts, registry.WithTransport(server.Client().Transport), registry.WithInsecureRegistries(hostOf(t, server)))
	}

	store, err := registry.Open(dir, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return store
}

func hostOf(t *testing.T, server *httptest.Server) string {
	t.Helper()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse the server URL: %v", err)
	}

	return parsed.Host
}

func pull(t *testing.T, store *registry.Store, ref string) registry.Image {
	t.Helper()

	img, err := store.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull %s: %v", ref, err)
	}

	return img
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

func TestPullRefusesPlaintextHTTP(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})

	// The same store, minus the opt-in: ggcr would downgrade a loopback or RFC1918 registry silently.
	store, err := registry.Open(t.TempDir(), registry.WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, err = store.Pull(t.Context(), ref)
	if err == nil {
		t.Fatal("Pull over plaintext http returned no error")
	}

	if !strings.Contains(err.Error(), "--insecure-registry") {
		t.Errorf("got %v, want an error that names --insecure-registry", err)
	}
}

func TestPullRefusesADotDotSegment(t *testing.T) {
	store := openStore(t, nil)

	_, err := store.Pull(t.Context(), "127.0.0.1:5000/../v2/app:1.0")
	if err == nil || !strings.Contains(err.Error(), "path segment") {
		t.Fatalf("got %v, want a rejected .. segment", err)
	}
}

func TestRemoveAcceptsTheDigestListPrints(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	store := openStore(t, server)

	pulled := pull(t, store, ref)

	byDigest := strings.Split(ref, ":")[0] + "@" + pulled.Digest
	if err := store.Remove(byDigest); err != nil {
		t.Fatalf("Remove by digest: %v", err)
	}

	if _, err := store.Get(ref); !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v after Remove, want ErrNotCached", err)
	}
}

func TestRemoveNamesTheTagsTheStoreHolds(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})
	store := openStore(t, server)

	pull(t, store, ref)

	// The tag-less form parses as :latest, which the store does not hold, so the miss must name what it does.
	repository := strings.Split(ref, ":1.0")[0]
	err := store.Remove(repository)
	if !errors.Is(err, registry.ErrNotCached) {
		t.Fatalf("got %v, want ErrNotCached", err)
	}

	if !strings.Contains(err.Error(), ref) {
		t.Errorf("got %v, want an error that names the held tag %s", err, ref)
	}
}

func TestRemoveSurvivesATempBlobFromAKilledPull(t *testing.T) {
	server, first := servedImage(t, "beta:1.0", map[string]string{"/beta": "1"})
	second := pushImage(t, server, "alpha:1.0", map[string]string{"/alpha": "1"})
	dir := t.TempDir()
	store := openStoreAt(t, dir, server)

	for _, ref := range []string{first, second} {
		pull(t, store, ref)
	}

	// ggcr writes a layer through os.CreateTemp(dir, hash.Hex), so a killed pull leaves a name like this.
	blobs := filepath.Join(dir, "blobs", "sha256")
	debris := filepath.Join(blobs, strings.Repeat("a", 64)+"1234567890")
	if err := os.WriteFile(debris, []byte("partial"), 0o600); err != nil {
		t.Fatalf("plant the temp blob: %v", err)
	}

	if err := store.Remove(first); err != nil {
		t.Fatalf("Remove with a temp blob present: %v", err)
	}

	if _, err := os.Stat(debris); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temp blob survived the collect: %v", err)
	}
}

func TestListDegradesOnAnUnreadableEntry(t *testing.T) {
	server, first := servedImage(t, "beta:1.0", map[string]string{"/beta": "1"})
	second := pushImage(t, server, "alpha:1.0", map[string]string{"/alpha": "1"})
	dir := t.TempDir()
	store := openStoreAt(t, dir, server)

	broken := pull(t, store, first)
	pull(t, store, second)

	hex := strings.TrimPrefix(broken.Digest, "sha256:")
	if err := os.Remove(filepath.Join(dir, "blobs", "sha256", hex)); err != nil {
		t.Fatalf("remove the manifest blob: %v", err)
	}

	images, err := store.List()
	if err != nil {
		t.Fatalf("List with one broken entry: %v", err)
	}

	if len(images) != 2 {
		t.Fatalf("got %d images, want both entries listed", len(images))
	}

	for _, img := range images {
		if (img.Reference == first) != (img.Broken != nil) {
			t.Errorf("%s: got Broken %v, want it set only on the damaged entry", img.Reference, img.Broken)
		}
	}

	// The healthy image must still be removable, which ggcr's collector was not able to do.
	if err := store.Remove(second); err != nil {
		t.Fatalf("Remove the healthy image: %v", err)
	}
}
