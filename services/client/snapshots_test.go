package client_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestCreateSnapshotPostsTheSandboxAndTheName(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusCreated, `{"id":"snap1","name":"base","source":"sandbox1"}`, &saw))

	snap, err := c.CreateSnapshot(t.Context(), sandbox.SnapshotRequest{Sandbox: "web", Name: "base"})
	if err != nil || snap.ID != "snap1" || snap.Name != "base" || snap.Source != "sandbox1" {
		t.Fatalf("CreateSnapshot = %+v, %v; want snap1 named base from sandbox1", snap, err)
	}
	if saw.method != http.MethodPost || saw.uri != "/v0/snapshots" || string(saw.body) != `{"sandbox":"web","name":"base"}` {
		t.Errorf("the request was %s %s with %q, want POST /v0/snapshots with the sandbox and the name", saw.method, saw.uri, saw.body)
	}
}

func TestTheSnapshotReadsAndTheRemoveNameTheRoute(t *testing.T) {
	var saw seen
	c := serve(t, shortRoot(t), echo(http.StatusOK, `{"snapshots":[{"id":"snap1"},{"id":"snap2"}],"next":null}`, &saw))

	snaps, err := c.ListSnapshots(t.Context())
	if err != nil || len(snaps) != 2 || snaps[1].ID != "snap2" {
		t.Fatalf("ListSnapshots = %+v, %v; want snap1 and snap2", snaps, err)
	}
	if saw.method != http.MethodGet || saw.uri != "/v0/snapshots" {
		t.Errorf("the list was %s %s, want GET /v0/snapshots", saw.method, saw.uri)
	}

	c = serve(t, shortRoot(t), echo(http.StatusOK, `{"id":"snap1","name":"base"}`, &saw))
	snap, err := c.InspectSnapshot(t.Context(), "base")
	if err != nil || snap.ID != "snap1" {
		t.Fatalf("InspectSnapshot = %+v, %v; want snap1", snap, err)
	}
	if saw.method != http.MethodGet || saw.uri != "/v0/snapshots/base" {
		t.Errorf("the inspect was %s %s, want GET /v0/snapshots/base", saw.method, saw.uri)
	}

	c = serve(t, shortRoot(t), echo(http.StatusNoContent, ``, &saw))
	if err := c.RemoveSnapshot(t.Context(), "base"); err != nil {
		t.Fatalf("RemoveSnapshot = %v", err)
	}
	if saw.method != http.MethodDelete || saw.uri != "/v0/snapshots/base" {
		t.Errorf("the remove was %s %s, want DELETE /v0/snapshots/base", saw.method, saw.uri)
	}
}

// NotFoundError prints "no sandbox", so a snapshot that is not there keeps the daemon's own line.
func TestASnapshotThatIsNotThereKeepsTheDaemonLine(t *testing.T) {
	const line = "snapshot ghost not found"
	c := serve(t, shortRoot(t), answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"`+line+`"}}`))

	calls := map[string]func() error{
		"inspect": func() error { _, err := c.InspectSnapshot(t.Context(), "ghost"); return err },
		"remove":  func() error { return c.RemoveSnapshot(t.Context(), "ghost") },
	}

	for verb, call := range calls {
		err := call()

		var notFound *client.NotFoundError
		var refusal *client.APIError
		if errors.As(err, &notFound) || !errors.As(err, &refusal) || refusal.Message != line {
			t.Errorf("%s = %v, want the APIError %q", verb, err, line)
		}
	}
}
