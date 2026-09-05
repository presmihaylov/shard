package sandbox_test

import (
	"errors"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

type fakeReader struct {
	rows       []models.Sandbox
	unreadable error
	names      map[string]string
	resolveErr error
}

func (f fakeReader) Get(id string) (models.Sandbox, error) {
	for _, sb := range f.rows {
		if sb.ID == id {
			return sb, nil
		}
	}

	return models.Sandbox{}, errors.New("sandbox " + id + ": not found")
}

func (f fakeReader) Resolve(ref string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if id, ok := f.names[ref]; ok {
		return id, nil
	}

	return ref, nil
}

func (f fakeReader) List() ([]models.Sandbox, error) { return f.rows, f.unreadable }

func rows() []models.Sandbox {
	return []models.Sandbox{
		{ID: "up-1", Name: "web", State: models.StateRunning},
		{ID: "down-2", State: models.StateStopped},
		{ID: "held-3", State: models.StatePaused},
	}
}

func TestListHidesTheStoppedSandboxesUnlessAll(t *testing.T) {
	broken := errors.New("decode sandbox.json of bad-4")
	repo := fakeReader{rows: rows(), unreadable: broken}

	got, err := sandbox.List(repo, false)
	if !errors.Is(err, broken) {
		t.Errorf("List dropped the unreadable error, got %v", err)
	}
	if len(got) != 2 || got[0].ID != "up-1" || got[1].ID != "held-3" {
		t.Errorf("List holds %v, want the running and the paused one", got)
	}

	got, err = sandbox.List(repo, true)
	if !errors.Is(err, broken) {
		t.Errorf("List with all dropped the unreadable error, got %v", err)
	}
	if len(got) != 3 {
		t.Errorf("List with all holds %v, want every row", got)
	}
}

func TestGetTakesAnIDOrAName(t *testing.T) {
	repo := fakeReader{rows: rows(), names: map[string]string{"web": "up-1"}}

	for _, ref := range []string{"up-1", "web"} {
		sb, err := sandbox.Get(repo, ref)
		if err != nil || sb.ID != "up-1" {
			t.Errorf("Get(%q) = %+v, %v; want up-1", ref, sb, err)
		}
	}

	if _, err := sandbox.Get(repo, "ghost"); err == nil {
		t.Error("Get of a reference nothing holds returned no error")
	}

	refused := errors.New("the sandbox id or name is empty")
	if _, err := sandbox.Get(fakeReader{resolveErr: refused}, ""); !errors.Is(err, refused) {
		t.Errorf("Get passed a Resolve error through as %v, want it unchanged", err)
	}
}
