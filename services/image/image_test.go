package image_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/presmihaylov/shard/services/image"
)

func TestPullUnpacksTheRootFS(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box", "bin/run": "#!/bin/sh\n"})
	svc := newService(t, server)

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(img.RootFS, "etc/hostname"))
	if err != nil {
		t.Fatalf("read the unpacked file: %v", err)
	}

	if string(got) != "box" {
		t.Errorf("got %q, want %q", got, "box")
	}

	if !slices.Equal(img.Config.Entrypoint, []string{"/bin/sh"}) {
		t.Errorf("got entrypoint %v, want [/bin/sh]", img.Config.Entrypoint)
	}

	if img.Created.IsZero() {
		t.Error("the image lost its created time")
	}
}

func TestPullAppliesWhiteouts(t *testing.T) {
	server, ref := servedImage(t, "app:1.0",
		map[string]string{"etc/hostname": "box", "etc/dropped": "gone"},
		map[string]string{"etc/.wh.dropped": ""},
	)
	svc := newService(t, server)

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if _, err := os.Stat(filepath.Join(img.RootFS, "etc/dropped")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the whiteout did not remove etc/dropped: %v", err)
	}

	if _, err := os.Stat(filepath.Join(img.RootFS, "etc/hostname")); err != nil {
		t.Errorf("the whiteout removed the wrong file: %v", err)
	}
}

func TestSecondPullNeedsNoNetwork(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	svc := newService(t, server)

	first, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("the first Pull: %v", err)
	}

	server.Close()

	second, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("the second Pull after the registry closed: %v", err)
	}

	if second.Digest != first.Digest || second.RootFS != first.RootFS {
		t.Errorf("the second pull returned %+v, want %+v", second, first)
	}
}

func TestPullLeavesNoDirectoryBehindWhenItFails(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	root := t.TempDir()
	svc := newServiceAt(t, root, server)

	if _, err := svc.Pull(t.Context(), ref+"-absent"); err == nil {
		t.Fatal("Pull of an absent tag returned no error")
	}

	entries, err := os.ReadDir(filepath.Join(root, "rootfs"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("got %d rootfs directories after a failed pull, want none", len(entries))
	}
}

func TestListAndRemove(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"etc/hostname": "box"})
	svc := newService(t, server)

	img, err := svc.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	images, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(images) != 1 || images[0].Reference != img.Reference {
		t.Fatalf("got %+v, want the one image we pulled", images)
	}

	if err := svc.Remove(ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(img.RootFS); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rootfs survived the removal: %v", err)
	}

	images, err = svc.List()
	if err != nil {
		t.Fatalf("List after Remove: %v", err)
	}

	if len(images) != 0 {
		t.Errorf("got %d images after Remove, want none", len(images))
	}
}

func TestRemoveKeepsARootFSASecondTagStillNeeds(t *testing.T) {
	files := map[string]string{"etc/hostname": "box"}
	server, first := servedImage(t, "app:1.0", files)
	second := pushImage(t, server, "app:latest", files)
	svc := newService(t, server)

	img, err := svc.Pull(t.Context(), first)
	if err != nil {
		t.Fatalf("Pull %s: %v", first, err)
	}

	if _, err := svc.Pull(t.Context(), second); err != nil {
		t.Fatalf("Pull %s: %v", second, err)
	}

	if err := svc.Remove(first); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(img.RootFS); err != nil {
		t.Errorf("the rootfs went while a second tag still names it: %v", err)
	}
}

func TestRemoveMissingImage(t *testing.T) {
	svc := newServiceAt(t, t.TempDir(), nil)

	if err := svc.Remove("app:1.0"); !errors.Is(err, image.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func newService(t *testing.T, server *httptest.Server) *image.Service {
	t.Helper()

	return newServiceAt(t, t.TempDir(), server)
}

func newServiceAt(t *testing.T, root string, server *httptest.Server) *image.Service {
	t.Helper()

	var opts []registry.Option
	if server != nil {
		opts = append(opts, registry.WithTransport(server.Client().Transport))
	}

	svc, err := image.New(root, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return svc
}

// servedImage starts an in-process registry and pushes one image, one layer per map of files.
func servedImage(t *testing.T, tag string, layers ...map[string]string) (*httptest.Server, string) {
	t.Helper()

	server := httptest.NewServer(ggcr.New())
	t.Cleanup(server.Close)

	return server, pushImage(t, server, tag, layers...)
}

func pushImage(t *testing.T, server *httptest.Server, tag string, layers ...map[string]string) string {
	t.Helper()

	host, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse the server URL: %v", err)
	}

	ref, err := name.ParseReference(host.Host + "/shard/" + tag)
	if err != nil {
		t.Fatalf("parse the reference: %v", err)
	}

	img := empty.Image
	for _, files := range layers {
		img, err = mutate.AppendLayers(img, tarLayer(t, files))
		if err != nil {
			t.Fatalf("append a layer: %v", err)
		}
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

// tarLayer builds a tar layer. A name that starts with .wh. is a whiteout, exactly as an image carries it.
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
