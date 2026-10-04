package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// A waited create of an uncached image answered 201 with the failed record when its refused app's sandbox stayed (SHARD-497).
func TestAWaitedCreateAnswersTheRemovalThatLeftARefusedSandbox(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", streamed), func(t *testing.T) {
			logged := &lockedWriter{}
			d := &deps{cfg: Config{Root: t.TempDir(), Out: logged, Provider: "gvisor"}}
			repo, err := d.repo()
			if err != nil {
				t.Fatalf("build the repository: %v", err)
			}

			svc := sandbox.New(sandbox.Config{Repo: undeletable{repo}, Images: pulledImage{rootfs: t.TempDir()}, Network: noNetwork{}, Provider: refusedApp{}})
			life := &lifecycle{deps: d, base: t.Context(), svc: svc}
			t.Cleanup(life.wait)
			server := httptest.NewServer(api.NewHandler("v-test", nil, repo, nil, life, nil, nil, nil, logged))
			t.Cleanup(server.Close)

			status, refusal := waitedCreate(t, server, streamed)

			want := http.StatusInternalServerError
			if streamed {
				want = http.StatusCreated
			}
			if status != want || refusal["code"] != string(models.CodeInternal) {
				t.Fatalf("the waited create answered %d with %v, want %d and the internal error", status, refusal, want)
			}
			if !strings.Contains(logged.String(), "was not removed") {
				t.Errorf("the daemon log lost the removal:\n%s", logged.String())
			}
		})
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

// undeletable refuses the delete, as a state dir the host will not let go of does.
type undeletable struct{ *sandboxstate.Repository }

func (undeletable) Delete(string) error { return errors.New("the state dir is busy") }

// pulledImage answers every pull at once with one event, so the create streams a line and reaches the start with no registry.
type pulledImage struct{ rootfs string }

func (p pulledImage) Pull(ctx context.Context, ref string) (image.Image, error) {
	image.ProgressFrom(ctx).Add(image.Event{Status: image.StatusCached, Reference: ref})

	return image.Image{RootFS: p.rootfs}, nil
}

func (pulledImage) Lookup(string) (image.Image, bool, error) { return image.Image{}, false, nil }

type noNetwork struct{}

func (noNetwork) Allocate(context.Context, string) (models.NetworkSpec, error) {
	return models.NetworkSpec{}, nil
}

func (noNetwork) Release(context.Context, string) error { return nil }

func (noNetwork) Reapply(context.Context, string) error { return nil }

func (noNetwork) ReapplyAll(context.Context) error { return nil }

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
