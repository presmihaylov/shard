package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
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

// A waited create of an uncached image answered 201 with the failed record when its refused app's sandbox stayed (SHARD-497).
func TestAWaitedCreateAnswersTheRemovalThatLeftARefusedSandbox(t *testing.T) {
	removals := map[string]error{
		"plain": errors.New("the state dir is busy"),
		// A typed removal error is the daemon log's to read, never a code the refusal answers with.
		"timeout": &sandbox.SubstrateTimeoutError{ID: "amber-otter-1a2b", Op: "remove", Budget: time.Second},
	}
	for name, removal := range removals {
		for _, streamed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/streamed=%t", name, streamed), func(t *testing.T) {
				logged := &lockedWriter{}
				d := &deps{cfg: Config{Root: t.TempDir(), Out: logged, Provider: "gvisor"}}
				repo, err := d.repo()
				if err != nil {
					t.Fatalf("build the repository: %v", err)
				}

				svc := sandbox.New(sandbox.Config{Repo: undeletable{repo, removal}, Images: pulledImages{}, Network: hostlessNetwork{}, Provider: refusedApp{}})
				life := &lifecycle{deps: d, base: t.Context(), svc: svc}
				t.Cleanup(life.wait)
				server := httptest.NewServer(api.NewHandler("v-test", nil, repo, nil, life, nil, nil, nil, logged))
				t.Cleanup(server.Close)

				status, refusal := waitedCreate(t, server, streamed)

				want := http.StatusInternalServerError
				if streamed {
					want = http.StatusCreated
				}
				stayed := strings.HasSuffix(fmt.Sprint(refusal["message"]), "and the sandbox was not removed")
				if status != want || refusal["code"] != string(models.CodeInternal) || !stayed {
					t.Fatalf("the waited create answered %d with %v, want %d and the internal error that says the sandbox stayed", status, refusal, want)
				}
				if !strings.Contains(logged.String(), removal.Error()) {
					t.Errorf("the daemon log lost the removal:\n%s", logged.String())
				}
			})
		}
	}
}

// waitedCreate posts a create with wait, and answers the status and the error object of the body or of its last line.
func waitedCreate(t *testing.T, server *httptest.Server, streamed bool) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v0/sandboxes?wait=true", strings.NewReader(`{"image":"alpine:3.20","command":["/no/such/app"]}`))
	if err != nil {
		t.Fatalf("build the create: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if streamed {
		req.Header.Set("Accept", "application/x-ndjson")
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST the create: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close the create body: %v", err)
		}
	}()

	var last map[string]any
	decoder := json.NewDecoder(resp.Body)
	for decoder.More() {
		var line map[string]any
		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("decode the create body: %v", err)
		}
		last = line
	}
	object, ok := last["error"].(map[string]any)
	if !ok {
		t.Fatalf("the create answered %d with %v, want an error object", resp.StatusCode, last)
	}

	return resp.StatusCode, object
}

// lockedWriter takes the handler's log and the background create's at once.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.buf.String()
}

// undeletable refuses the delete with err, as a state dir the host will not let go of does.
type undeletable struct {
	*sandboxstate.Repository

	err error
}

func (u undeletable) Delete(string) error { return u.err }

// refusedApp is a substrate whose start reports an app that never ran.
type refusedApp struct{ models.Provider }

func (refusedApp) Name() string { return "fake" }

func (refusedApp) CheckResources(models.Resources) error { return nil }

func (refusedApp) Create(context.Context, models.SandboxSpec) error { return nil }

func (refusedApp) Status(context.Context, string) (models.Status, error) {
	return models.Status{}, nil
}

func (refusedApp) Start(_ context.Context, id string) error {
	return &models.CommandNotStartedError{Sandbox: id, Reason: "no such file or directory", Code: models.CommandNotFoundExitCode}
}

func (refusedApp) Remove(context.Context, string) error { return nil }
