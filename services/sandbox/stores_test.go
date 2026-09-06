package sandbox_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// fakePolicies is the policy store the show and rm verbs drive.
type fakePolicies struct {
	policy  models.Policy
	missing bool
	removed string
}

func (f *fakePolicies) Set(policy models.Policy) error { f.policy = policy; return nil }

func (f *fakePolicies) Get(name string) (models.Policy, error) {
	if f.missing || name != f.policy.Name {
		return models.Policy{}, errors.New("no such policy")
	}

	return f.policy, nil
}

func (f *fakePolicies) List() ([]models.Policy, error) { return []models.Policy{f.policy}, nil }

func (f *fakePolicies) Remove(name string) error { f.removed = name; return nil }

func heldBy(t *testing.T, records []models.Sandbox) (*sandbox.Stores, *fakePolicies) {
	t.Helper()

	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	repo := &fakeRepo{r: &recorder{}, left: records}

	return sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies}), policies
}

func TestPolicyShowNamesTheSandboxesThatHoldIt(t *testing.T) {
	stores, _ := heldBy(t, []models.Sandbox{
		{ID: "sb-1", Policy: "web"},
		{ID: "sb-2", Policy: "db"},
		{ID: "sb-3", Policy: "web"},
	})

	view, err := stores.Policy("web")
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}

	if view.Name != "web" {
		t.Errorf("the view names policy %q", view.Name)
	}
	if !slices.Equal(view.Holders, []string{"sb-1", "sb-3"}) {
		t.Errorf("the holders are %v, want the two records that name the policy", view.Holders)
	}
}

func TestPolicyShowOmitsTheHoldersWhenNobodyHoldsIt(t *testing.T) {
	stores, _ := heldBy(t, []models.Sandbox{{ID: "sb-1", Policy: "db"}})

	view, err := stores.Policy("web")
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}

	if view.Holders != nil {
		t.Errorf("the holders are %v, want none: the field is omitted from the JSON", view.Holders)
	}
}

// show and rm read the records through one helper, so a holder rm refuses is a holder show prints.
func TestPolicyShowAndPolicyRemoveAgreeOnWhoHoldsIt(t *testing.T) {
	stores, policies := heldBy(t, []models.Sandbox{{ID: "sb-1", Policy: "web"}})

	view, err := stores.Policy("web")
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}

	err = stores.RemovePolicy("web")
	var held *sandbox.HeldError
	if !errors.As(err, &held) {
		t.Fatalf("RemovePolicy answered %v, want a refusal", err)
	}
	if !slices.Equal(held.Users, view.Holders) {
		t.Errorf("rm refuses over %v and show prints %v", held.Users, view.Holders)
	}
	if policies.removed != "" {
		t.Errorf("the refusal still removed policy %q", policies.removed)
	}
}

// A record that does not read back may name the policy, so show refuses rather than print a short list.
func TestPolicyShowFailsWhenARecordDoesNotReadBack(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	repo := &fakeRepo{r: &recorder{fail: []string{"repo.List"}}}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies})

	if _, err := stores.Policy("web"); err == nil {
		t.Fatal("Policy answered a view over records it could not read")
	}
}
