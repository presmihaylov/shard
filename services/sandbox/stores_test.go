package sandbox_test

import (
	"errors"
	"slices"
	"strings"
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

// The policy is stored by the time the holders are read, so the failure says so rather than leaving the
// operator to guess whether it landed.
func TestSetPolicySaysThePolicyLandedWhenItCannotTellWhoHoldsIt(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	repo := &fakeRepo{r: &recorder{fail: []string{"repo.List"}}}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies})

	_, err := stores.SetPolicy(t.Context(), "web", sandbox.PolicyRequest{})
	if err == nil {
		t.Fatal("SetPolicy answered over records it could not read")
	}
	if !strings.Contains(err.Error(), "policy web is stored") {
		t.Errorf("SetPolicy failed with %q, which does not say the policy landed", err)
	}
	if policies.policy.Name != "web" {
		t.Error("the policy was not stored before the holders were read")
	}
}

// Nothing stores whether a policy resolves, so the view computes it and show and create agree.
func TestPolicyViewSaysWhetherDNSIsOpen(t *testing.T) {
	for _, tc := range []struct {
		rule models.Rule
		want string
	}{
		{models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationDomain, Value: "api.example.com"}, Protocol: "tcp", Ports: []int{443}}, "open"},
		{models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationGroup, Value: "dns"}}, "open"},
		{models.Rule{Action: models.ActionAllow, Destination: models.Destination{Kind: models.DestinationCIDR, Value: "203.0.113.7/32"}}, "closed"},
	} {
		policy := models.Policy{Name: "web", Rules: []models.Rule{tc.rule}}
		stores := sandbox.NewStores(sandbox.StoresConfig{Repo: &fakeRepo{r: &recorder{}}, Policies: &fakePolicies{policy: policy}})

		view, err := stores.Policy("web")
		if err != nil {
			t.Fatalf("Policy: %v", err)
		}
		if view.DNS != tc.want {
			t.Errorf("the view of %+v says dns is %q, want %q", tc.rule, view.DNS, tc.want)
		}
	}
}
