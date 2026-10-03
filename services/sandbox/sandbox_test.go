package sandbox_test

import (
	"errors"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
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

// fakeEnforcer answers Effective for whatever policy the record names, and counts the asks.
type fakeEnforcer struct {
	asked int
}

func (f *fakeEnforcer) Effective(sb models.Sandbox) (egress.Effective, error) {
	f.asked++

	return egress.Effective{Policy: sb.Policy}, nil
}

func TestInspectAsksTheEnforcerOnlyForARecordThatNamesAPolicy(t *testing.T) {
	enforcer := &fakeEnforcer{}
	repo := fakeReader{rows: []models.Sandbox{{ID: "up-1", Name: "web", Policy: "deny-all"}, {ID: "up-2"}}, names: map[string]string{"web": "up-1"}}

	got, err := sandbox.Inspect(repo, enforcer, "web")
	if err != nil || got.ID != "up-1" || got.Egress == nil || got.Egress.Policy != "deny-all" {
		t.Errorf("Inspect(web) = %+v, %v; want up-1 with its egress", got, err)
	}

	got, err = sandbox.Inspect(repo, enforcer, "up-2")
	if err != nil || got.ID != "up-2" || got.Egress != nil {
		t.Errorf("Inspect(up-2) = %+v, %v; want up-2 with no egress", got, err)
	}
	if enforcer.asked != 1 {
		t.Errorf("the enforcer was asked %d times, want once, for the record that names a policy", enforcer.asked)
	}

	if _, err := sandbox.Inspect(repo, enforcer, "ghost"); err == nil {
		t.Error("Inspect of a reference nothing holds returned no error")
	}
}
