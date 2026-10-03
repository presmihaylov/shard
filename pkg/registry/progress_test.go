package registry_test

import (
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	ggcr "github.com/google/go-containerregistry/pkg/registry"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// heard is what a pull told its Progress, for a test to read back once the pull returned.
type heard struct {
	mu     sync.Mutex
	ref    string
	digest string
	sizes  []int64
	layers []heardLayer
}

type heardLayer struct {
	digest  string
	size    int64
	present bool
}

func (h *heard) Manifest(ref, digest string, layers []int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.ref, h.digest, h.sizes = ref, digest, layers
}

func (h *heard) Layer(digest string, size int64, present bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.layers = append(h.layers, heardLayer{digest: digest, size: size, present: present})
}

func (h *heard) layer(t *testing.T, digest string) heardLayer {
	t.Helper()

	i := slices.IndexFunc(h.layers, func(l heardLayer) bool { return l.digest == digest })
	if i < 0 {
		t.Fatalf("no report for layer %s in %+v", digest, h.layers)
	}

	return h.layers[i]
}

func TestPullReportsEachLayerAsItLands(t *testing.T) {
	server := httptest.NewServer(ggcr.New())
	t.Cleanup(server.Close)

	base := tarLayer(t, map[string]string{"/etc/hostname": "box"})
	extra := tarLayer(t, map[string]string{"/etc/motd": "hello"})
	first := pushLayers(t, server, "app:1.0", base)
	second := pushLayers(t, server, "app:2.0", base, extra)
	store := openStore(t, server)

	var one heard
	pulled, err := store.Pull(t.Context(), first, &one)
	if err != nil {
		t.Fatalf("Pull %s: %v", first, err)
	}

	if one.ref != first || one.digest != pulled.Digest || len(one.sizes) != 1 || one.sizes[0] != sizeOf(t, base) {
		t.Errorf("the manifest report was %s %s %v, want %s %s with one layer of %d bytes", one.ref, one.digest, one.sizes, first, pulled.Digest, sizeOf(t, base))
	}
	if got := one.layer(t, digestOf(t, base)); got.present || got.size != sizeOf(t, base) || len(one.layers) != 1 {
		t.Errorf("the layer reports were %+v, want one fetch of %d bytes", one.layers, sizeOf(t, base))
	}

	// The second image shares the first's layer, so only its own layer costs a download.
	var two heard
	if _, err := store.Pull(t.Context(), second, &two); err != nil {
		t.Fatalf("Pull %s: %v", second, err)
	}

	if len(two.sizes) != 2 || len(two.layers) != 2 {
		t.Fatalf("the second pull reported %v and %+v, want two layers", two.sizes, two.layers)
	}
	if got := two.layer(t, digestOf(t, base)); !got.present || got.size != sizeOf(t, base) {
		t.Errorf("the shared layer reported %+v, want it present at %d bytes", got, sizeOf(t, base))
	}
	if got := two.layer(t, digestOf(t, extra)); got.present || got.size != sizeOf(t, extra) {
		t.Errorf("the new layer reported %+v, want a fetch of %d bytes", got, sizeOf(t, extra))
	}
}

func pushLayers(t *testing.T, server *httptest.Server, tag string, layers ...v1.Layer) string {
	t.Helper()

	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatalf("append the layers: %v", err)
	}

	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		OS:           "linux",
		Architecture: "amd64",
		Created:      v1.Time{Time: time.Unix(1700000000, 0).UTC()},
		Config:       v1.Config{Entrypoint: []string{"/bin/sh"}},
	})
	if err != nil {
		t.Fatalf("set the config: %v", err)
	}

	ref, err := name.ParseReference(hostOf(t, server) + "/shard/" + tag)
	if err != nil {
		t.Fatalf("parse the reference: %v", err)
	}

	if err := remote.Write(ref, img, remote.WithTransport(server.Client().Transport)); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}

	return ref.Name()
}

func sizeOf(t *testing.T, layer v1.Layer) int64 {
	t.Helper()

	size, err := layer.Size()
	if err != nil {
		t.Fatalf("size the layer: %v", err)
	}

	return size
}

func digestOf(t *testing.T, layer v1.Layer) string {
	t.Helper()

	digest, err := layer.Digest()
	if err != nil {
		t.Fatalf("digest the layer: %v", err)
	}

	return digest.String()
}
