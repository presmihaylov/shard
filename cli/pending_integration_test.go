//go:build integration

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// unpullableImage is a well-formed reference no registry holds, so its pull fails in the background
// and the record lands failed rather than the create answering an error.
const unpullableImage = "alpine:no-such-tag-shard-pr12"

// TestCreateFromACachedImageAnswersRunningAtOnce is the cached fast path: an image already pulled
// needs no download, so the create builds and starts the sandbox before it answers and never sits pending.
func TestCreateFromACachedImageAnswersRunningAtOnce(t *testing.T) {
	app, _ := newCreateApp(t)

	if err := app.Run(t.Context(), []string{"pull", testImage}); err != nil {
		t.Fatalf("pull %s: %v", testImage, err)
	}

	sb, err := daemonClient(app).CreateSandbox(t.Context(), sandbox.CreateRequest{
		Image:   testImage,
		Command: []string{"/bin/sleep", "600"},
	})
	if err != nil {
		t.Fatalf("create from a cached image: %v", err)
	}
	t.Cleanup(func() { cleanUp(t, app, sb.ID) })

	if sb.State != models.StateRunning {
		t.Errorf("the create answered %q, want running: a cached image needs no background pull", sb.State)
	}
}

// TestCreateFromAnUncachedImageIsPendingThenRunning is the async path: the create answers pending and
// the daemon pulls and starts behind the record, which then reaches running.
func TestCreateFromAnUncachedImageIsPendingThenRunning(t *testing.T) {
	// A daemon of this test's own has an empty image tree, so the image is uncached and the create goes async.
	app, _ := ownDaemon(t)

	sb, err := daemonClient(app).CreateSandbox(t.Context(), sandbox.CreateRequest{
		Image:   testImage,
		Command: []string{"/bin/sleep", "600"},
	})
	if err != nil {
		t.Fatalf("create from an uncached image: %v", err)
	}
	if sb.State != models.StatePending {
		t.Fatalf("the create answered %q, want pending: an uncached image pulls in the background", sb.State)
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	final, err := daemonClient(app).WaitSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatalf("wait for the create to finish: %v", err)
	}
	if final.State != models.StateRunning {
		t.Errorf("the sandbox reached %q, want running; failed_reason=%q", final.State, final.FailedReason)
	}
}

// TestCreateFromAnUnpullableImageEndsFailed is the failed path and its guard: the create answers
// pending, the background pull fails, the record lands failed with the reason, every verb but a get
// and an rm is refused with sandbox_failed, and rm frees it.
func TestCreateFromAnUnpullableImageEndsFailed(t *testing.T) {
	app, _ := newCreateApp(t)

	sb, err := daemonClient(app).CreateSandbox(t.Context(), sandbox.CreateRequest{
		Image:   unpullableImage,
		Command: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("create from an unpullable image: %v", err)
	}
	t.Cleanup(func() { cleanUp(t, app, sb.ID) })

	if sb.State != models.StatePending {
		t.Fatalf("the create answered %q, want pending", sb.State)
	}

	ctx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()

	final, err := daemonClient(app).WaitSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatalf("wait for the create to fail: %v", err)
	}
	if final.State != models.StateFailed {
		t.Fatalf("the sandbox reached %q, want failed", final.State)
	}
	if final.FailedReason == "" || strings.Contains(final.FailedReason, "\n") {
		t.Errorf("the record holds the failed_reason %q, want one non-empty line", final.FailedReason)
	}

	// Every verb but a get and an rm is refused, and the code names the state.
	_, err = daemonClient(app).StartSandbox(t.Context(), sb.ID)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != models.CodeSandboxFailed {
		t.Errorf("start of a failed sandbox = %v, want 409 %s", err, models.CodeSandboxFailed)
	}

	// rm frees it without force: a failed create holds no live process, so the record and everything under it goes.
	if err := daemonClient(app).RemoveSandbox(t.Context(), sb.ID, false, stopGrace); err != nil {
		t.Fatalf("rm of a failed sandbox: %v", err)
	}

	_, err = daemonClient(app).GetSandbox(t.Context(), sb.ID)
	var missing *client.NotFoundError
	if !errors.As(err, &missing) {
		t.Errorf("get after rm = %v, want not found", err)
	}
}
