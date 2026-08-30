package cli

import (
	"bytes"
	"context"
	"errors"
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

func (f *fakeImageStore) Remove(_ context.Context, ref string) error {
	f.removed = append(f.removed, ref)

	return nil
}

const alpine = "index.docker.io/library/alpine:3.20"

// newImageApp wires image rm and prune onto two pulled images and the sandboxes in left.
func newImageApp(t *testing.T, out *bytes.Buffer, left []models.Sandbox) (App, *fakeImageStore) {
	t.Helper()

	app, d := newLifecycleApp(t, out, &recorder{}, models.Sandbox{})
	d.repoSvc.(*fakeLifecycleRepo).left = left

	store := &fakeImageStore{images: []image.Image{
		{Reference: alpine, Digest: "sha256:aaa"},
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
