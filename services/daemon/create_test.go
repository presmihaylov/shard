package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// refusingProvider fails every create with err, which a test sets to what the bundle meets after the pull.
type refusingProvider struct {
	models.Provider

	err error
}

func (refusingProvider) Name() string                                       { return "gvisor" }
func (refusingProvider) CheckResources(models.Resources) error              { return nil }
func (refusingProvider) Capabilities() models.Capabilities                  { return models.Capabilities{} }
func (p refusingProvider) Create(context.Context, models.SandboxSpec) error { return p.err }
func (refusingProvider) Remove(context.Context, string) error               { return nil }

// pulledImages answers every pull at once with one event, so the create reaches the substrate with no registry and a streamed one writes a line.
type pulledImages struct{}

func (pulledImages) Pull(ctx context.Context, ref string) (image.Image, error) {
	image.ProgressFrom(ctx).Add(image.Event{Status: image.StatusCached, Reference: ref})

	return image.Image{RootFS: "/rootfs"}, nil
}

func (pulledImages) Lookup(string) (image.Image, bool, error) { return image.Image{}, false, nil }

// hostlessNetwork hands out and frees an address without touching the host.
type hostlessNetwork struct{}

func (hostlessNetwork) Allocate(context.Context, string) (models.NetworkSpec, error) {
	return models.NetworkSpec{}, nil
}

func (hostlessNetwork) Release(context.Context, string) error { return nil }
func (hostlessNetwork) Reapply(context.Context, string) error { return nil }
func (hostlessNetwork) ReapplyAll(context.Context) error      { return nil }

// An uncached image goes to the background, where the user is read only after the pull: a waited create still answers the refusal.
func TestAWaitedCreateOffAnUncachedImageRefusesAnUnknownUser(t *testing.T) {
	root := t.TempDir()
	repo, err := sandboxstate.New(root)
	if err != nil {
		t.Fatal(err)
	}
	images, err := image.New(filepath.Join(root, "images"))
	if err != nil {
		t.Fatal(err)
	}

	unknown := &bundle.UnknownUserError{Err: errors.New(`resolve the user "nobody2": no such entry in the image`)}
	d := &deps{cfg: Config{Root: root}, repoSvc: repo, imageSvc: images}
	l := &lifecycle{deps: d, base: t.Context(), svc: sandbox.New(sandbox.Config{
		Repo:     repo,
		Images:   pulledImages{},
		Network:  hostlessNetwork{},
		Provider: refusingProvider{err: fmt.Errorf("build the bundle under %s: %w", root, unknown)},
	})}
	t.Cleanup(l.wait)
	req := sandbox.CreateRequest{Image: "alpine:3.20", User: "nobody2"}

	_, err = l.CreateAndWait(t.Context(), req)
	refused, ok := errors.AsType[*sandbox.RequestError](err)
	if !ok || refused.Public() != unknown.Error() {
		t.Fatalf("the waited create answered %v, want the request refused with %q", err, unknown.Error())
	}

	sb, err := l.Create(t.Context(), req)
	if err != nil || sb.State != models.StatePending {
		t.Fatalf("the plain create answered %+v, %v, want the pending record", sb, err)
	}
	l.wait()

	failed, err := repo.Get(sb.ID)
	if err != nil || failed.State != models.StateFailed || failed.FailedPublic != unknown.Error() {
		t.Errorf("the record is %+v, %v, want failed with the user named", failed, err)
	}
}
