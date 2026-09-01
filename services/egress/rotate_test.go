package egress

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

func rotationEvents(t *testing.T) (*Events, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sb"), 0o700); err != nil {
		t.Fatal(err)
	}
	events := NewEvents(func(id string) (string, error) { return filepath.Join(root, id), nil })

	return events, filepath.Join(root, "sb")
}

func TestRotateLeavesASmallFileAndAMissingOneAlone(t *testing.T) {
	events, dir := rotationEvents(t)

	if err := events.Rotate("sb"); err != nil {
		t.Fatalf("Rotate with no file: %v", err)
	}

	ev := models.EgressEvent{Time: time.Unix(10, 0).UTC(), Sandbox: "sb", Verdict: models.ActionAllow}
	if err := events.Record(ev); err != nil {
		t.Fatal(err)
	}
	if err := events.Rotate("sb"); err != nil {
		t.Fatalf("Rotate under the bound: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, rotatedFile)); err == nil {
		t.Error("a file under the bound was rotated")
	}
}

func TestRotateMovesAnOversizedFileAndReadSpansBothGenerations(t *testing.T) {
	events, dir := rotationEvents(t)
	events.maxBytes = 1

	old := models.EgressEvent{Time: time.Unix(10, 0).UTC(), Sandbox: "sb", Verdict: models.ActionDeny}
	if err := events.Record(old); err != nil {
		t.Fatal(err)
	}
	if err := events.Rotate("sb"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, eventsFile)); !os.IsNotExist(err) {
		t.Errorf("the live file after a rotation: %v", err)
	}

	fresh := models.EgressEvent{Time: time.Unix(11, 0).UTC(), Sandbox: "sb", Verdict: models.ActionAllow}
	if err := events.Record(fresh); err != nil {
		t.Fatal(err)
	}

	got, err := events.Read("sb")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 || got[0] != old || got[1] != fresh {
		t.Errorf("Read = %+v, want the rotated event then the fresh one", got)
	}

	// A second rotation replaces the kept generation, so the bound holds.
	if err := events.Rotate("sb"); err != nil {
		t.Fatalf("the second Rotate: %v", err)
	}
	got, err = events.Read("sb")
	if err != nil {
		t.Fatalf("Read after the second rotation: %v", err)
	}
	if len(got) != 1 || got[0] != fresh {
		t.Errorf("Read = %+v, want only the fresh event kept", got)
	}
}
