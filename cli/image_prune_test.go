package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
)

// fakeImageStore answers List with what it holds and records what Remove took.
type fakeImageStore struct {
	imageService

	images  []image.Image
	removed []string
}

func (f *fakeImageStore) List() ([]image.Image, error) { return f.images, nil }

// Orphaned answers as the store would: a removal frees a digest when no other entry in f holds it.
func (f *fakeImageStore) Orphaned(ref string) ([]string, error) {
	canonical, err := image.Canonical(ref)
	if err != nil {
		return nil, err
	}

	var matched, rest []image.Image
	for _, img := range f.images {
		if img.Reference == canonical || strings.HasSuffix(canonical, "@"+img.Digest) {
			matched = append(matched, img)

			continue
		}

		rest = append(rest, img)
	}

	var orphaned []string
	for _, img := range matched {
		if !slices.ContainsFunc(rest, func(r image.Image) bool { return r.Digest == img.Digest }) {
			orphaned = append(orphaned, img.Digest)
		}
	}

	return orphaned, nil
}

func (f *fakeImageStore) Remove(_ context.Context, ref string, free func() error) error {
	if err := free(); err != nil {
		return err
	}
	f.removed = append(f.removed, ref)

	return nil
}

const alpine = "index.docker.io/library/alpine:3.20"

// alpineDigest is what the alpine entry hashes to, in the length the reference parser insists on.
const alpineDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// newImageApp wires image rm and prune onto two pulled images and the sandboxes in left.
func newImageApp(t *testing.T, out *bytes.Buffer, left []models.Sandbox) (App, *fakeImageStore) {
	t.Helper()

	app, d := newLifecycleApp(t, out, &recorder{}, models.Sandbox{})
	d.repoSvc.(*fakeLifecycleRepo).left = left

	store := &fakeImageStore{images: []image.Image{
		{Reference: alpine, Digest: alpineDigest},
		{Reference: "index.docker.io/library/python:3.12-alpine", Digest: "sha256:bbb"},
	}}
	d.imageSvc = store

	return app, store
}

func TestImageRmRefusesAnImageASandboxReferences(t *testing.T) {
	var out bytes.Buffer

	held := []models.Sandbox{{ID: "down-2", Image: alpine, State: models.StateStopped}}
	app, store := newImageApp(t, &out, held)

	err := app.Run(t.Context(), []string{"image", "rm", "alpine:3.20"})
	if err == nil || !strings.Contains(err.Error(), "down-2") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("image rm returned %v, want a refusal that names the sandbox and --force", err)
	}
	if len(store.removed) != 0 {
		t.Errorf("image rm removed %v under a sandbox", store.removed)
	}
}

func TestImageRmForceRemovesAnImageASandboxReferences(t *testing.T) {
	var out bytes.Buffer

	held := []models.Sandbox{{ID: "up-1", Image: alpine, State: models.StateRunning}}
	app, store := newImageApp(t, &out, held)

	if err := app.Run(t.Context(), []string{"image", "rm", "--force", "alpine:3.20"}); err != nil {
		t.Fatalf("image rm --force: %v", err)
	}
	if len(store.removed) != 1 || store.removed[0] != "alpine:3.20" {
		t.Errorf("image rm --force removed %v", store.removed)
	}
}

func TestImageRmRemovesAnImageNoSandboxReferences(t *testing.T) {
	var out bytes.Buffer

	held := []models.Sandbox{{ID: "up-1", Image: "index.docker.io/library/python:3.12-alpine", State: models.StateRunning}}
	app, store := newImageApp(t, &out, held)

	if err := app.Run(t.Context(), []string{"image", "rm", "alpine:3.20"}); err != nil {
		t.Fatalf("image rm: %v", err)
	}
	if len(store.removed) != 1 {
		t.Errorf("image rm removed %v", store.removed)
	}
}

func TestImagePruneKeepsWhatAStoppedSandboxReferences(t *testing.T) {
	var out bytes.Buffer

	held := []models.Sandbox{{ID: "down-2", Image: alpine, State: models.StateStopped}}
	app, store := newImageApp(t, &out, held)

	if err := app.Run(t.Context(), []string{"image", "prune"}); err != nil {
		t.Fatalf("image prune: %v", err)
	}
	if len(store.removed) != 1 || store.removed[0] != "index.docker.io/library/python:3.12-alpine" {
		t.Errorf("image prune removed %v, want the python image alone", store.removed)
	}
	if got := strings.TrimSpace(out.String()); got != "index.docker.io/library/python:3.12-alpine" {
		t.Errorf("image prune printed %q", got)
	}
}

func TestImagePruneRefusesToGuessOverAnUnreadableRecord(t *testing.T) {
	var out bytes.Buffer

	app, store := newImageApp(t, &out, nil)
	app.newDeps(app).repoSvc.(*fakeLifecycleRepo).unreadable = errors.New("decode sandbox.json of bad-3: unexpected end of JSON input")

	if err := app.Run(t.Context(), []string{"image", "prune"}); err == nil {
		t.Error("image prune returned no error over a record it could not read")
	}
	if len(store.removed) != 0 {
		t.Errorf("image prune removed %v over a record it could not read", store.removed)
	}
}

// A digest reference names every tag of that digest, so the name check alone would let it through.
func TestImageRmRefusesADigestASandboxHoldsByTag(t *testing.T) {
	var out bytes.Buffer

	held := []models.Sandbox{{ID: "up-1", Image: alpine, State: models.StateRunning}}
	app, store := newImageApp(t, &out, held)

	err := app.Run(t.Context(), []string{"image", "rm", "alpine@" + alpineDigest})
	if err == nil || !strings.Contains(err.Error(), "up-1") {
		t.Fatalf("image rm by digest returned %v, want a refusal that names the sandbox", err)
	}
	if len(store.removed) != 0 {
		t.Errorf("image rm removed %v under a running sandbox", store.removed)
	}
}

func TestImagePruneKeepsADigestEntryATagStillHolds(t *testing.T) {
	var out bytes.Buffer

	held := []models.Sandbox{{ID: "up-1", Image: alpine, State: models.StateRunning}}
	app, store := newImageApp(t, &out, held)
	store.images = append(store.images, image.Image{Reference: "index.docker.io/library/alpine@" + alpineDigest, Digest: alpineDigest})

	if err := app.Run(t.Context(), []string{"image", "prune"}); err != nil {
		t.Fatalf("image prune: %v", err)
	}

	if slices.Contains(store.removed, "index.docker.io/library/alpine@"+alpineDigest) {
		t.Errorf("prune removed the digest entry, and the running sandbox's rootfs with it: %v", store.removed)
	}
}

func TestImageRmNamesTheFlagOrder(t *testing.T) {
	_, err := parseImageRemove([]string{"alpine:3.20", "--force"})
	if err == nil || !strings.Contains(err.Error(), "before the image") {
		t.Errorf("parseImageRemove returned %v, want the flag order named", err)
	}
}
