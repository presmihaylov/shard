package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// A snapshot goes from a stopped sandbox to a new one: create prints its id, list and inspect read it back, create --snapshot starts from it, and remove deletes it.
func TestASnapshotSeedsANewSandboxFromAStoppedOne(t *testing.T) {
	var out bytes.Buffer

	source := stopped()
	// A record holds its image normalized, as a create wrote it.
	source.Image = "index.docker.io/library/alpine:3.20"
	source.Digest = "sha256:alpine"
	app, d := newClientApp(t, &out, source)
	d.imageSvc = fakeImages{r: &recorder{}}

	if err := app.Run(t.Context(), []string{"snapshot", "create", "--name", "web-base", "web"}); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	id := strings.TrimSpace(out.String())
	if id == "" || strings.ContainsAny(id, " \n") {
		t.Fatalf("snapshot create printed %q, want the bare id", out.String())
	}
	if got := d.providerSvc.(*fakeLifecycleProvider).snapshotFrom; got != source.ID {
		t.Errorf("the provider copied %q, want the source %s", got, source.ID)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"snapshot", "list"}); err != nil {
		t.Fatalf("snapshot list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.HasPrefix(lines[1], id) || !strings.Contains(lines[1], "web-base") {
		t.Errorf("snapshot list printed %q, want the header and the one snapshot", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"snapshot", "inspect", "web-base"}); err != nil {
		t.Fatalf("snapshot inspect: %v", err)
	}
	var snap models.Snapshot
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatalf("snapshot inspect printed %q, which is not a snapshot: %v", out.String(), err)
	}
	if snap.ID != id || snap.Source != source.ID || snap.Image != source.Image || snap.Provider != "fake" {
		t.Errorf("snapshot inspect = %+v, want the id, the source, its image and the provider", snap)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"create", "--snapshot", "web-base"}); err != nil {
		t.Fatalf("create --snapshot: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox2" {
		t.Errorf("create --snapshot printed %q, want the new id", got)
	}
	if got := d.repoSvc.(*fakeLifecycleRepo).created; got.Snapshot != id || got.Image != source.Image {
		t.Errorf("create --snapshot recorded %+v, want the snapshot and the image it names", got)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"snapshot", "remove", "web-base"}); err != nil {
		t.Fatalf("snapshot remove: %v", err)
	}
	if err := app.Run(t.Context(), []string{"snapshot", "inspect", id}); err == nil {
		t.Errorf("snapshot inspect after remove found %s, want it gone", id)
	}
}

// A paused source still holds memory a copy of its files would miss, so the daemon refuses it and the CLI prints the fix.
func TestSnapshotCreateRefusesASourceThatIsNotStopped(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, paused())

	err := app.Run(t.Context(), []string{"snapshot", "create", "web"})
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("snapshot create of a paused sandbox = %v, want the fix to stop it first", err)
	}
	if got := d.providerSvc.(*fakeLifecycleProvider).snapshotFrom; got != "" {
		t.Errorf("the provider copied %q, want nothing", got)
	}
	if out.Len() != 0 {
		t.Errorf("the refused snapshot create printed %q", out.String())
	}
}
