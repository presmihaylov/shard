package sandbox_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
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

// fakeCompiler is the host compile, which refuses with err when set.
type fakeCompiler struct{ err error }

func (f fakeCompiler) Compiles(context.Context, models.Policy) error { return f.err }

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

// A scan that fails outright cannot name the holders, so show refuses rather than print a short list.
func TestPolicyShowFailsWhenTheRecordsCannotBeListed(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	repo := &fakeRepo{r: &recorder{fail: []string{"repo.List"}}}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies})

	if _, err := stores.Policy("web"); err == nil {
		t.Fatal("Policy answered a view over records it could not read")
	}
}

// unreadable is what List answers while the records of ids do not decode.
func unreadable(ids ...string) error {
	var err error
	for _, id := range ids {
		err = errors.Join(err, &sandboxstate.UnreadableError{ID: id, Err: errors.New("unexpected end of JSON input")})
	}

	return err
}

// A record that does not read back may hold the policy, so rm refuses and names it beside the readable holders (SHARD-584).
func TestPolicyRemoveRefusesWhileARecordDoesNotReadBack(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	left := []models.Sandbox{{ID: "sb-1", Policy: "web"}, {ID: "sb-2", Policy: "db"}}
	repo := &fakeRepo{r: &recorder{}, left: left, listErr: unreadable("broken-1", "broken-2")}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies})

	err := stores.RemovePolicy("web")
	var held *sandbox.HeldError
	if !errors.As(err, &held) || !slices.Equal(held.Users, []string{"sb-1", "broken-1", "broken-2"}) {
		t.Fatalf("RemovePolicy = %v, want a refusal that names the holder and the records it could not read", err)
	}
	if !strings.Contains(err.Error(), "unreadable record of broken-1, broken-2") {
		t.Errorf("the refusal reads %q, which does not say which records are unknown", err.Error())
	}
	if policies.removed != "" {
		t.Errorf("the refusal still removed policy %q", policies.removed)
	}
}

// A record that does not read back may name the policy, so show lists it beside the readable holders, as rm refuses over (SHARD-597).
func TestPolicyShowNamesTheRecordsThatDoNotReadBack(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	left := []models.Sandbox{{ID: "sb-1", Policy: "web"}, {ID: "sb-2", Policy: "db"}}
	repo := &fakeRepo{r: &recorder{}, left: left, listErr: unreadable("broken-1")}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies})

	view, err := stores.Policy("web")
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if !slices.Equal(view.Holders, []string{"sb-1", "broken-1"}) {
		t.Errorf("show prints holders %v, want the holder and the record it could not read", view.Holders)
	}

	err = stores.RemovePolicy("web")
	if held, ok := errors.AsType[*sandbox.HeldError](err); !ok || !slices.Equal(held.Users, view.Holders) {
		t.Errorf("rm answers %v and show prints %v", err, view.Holders)
	}
}

// Anything else that fails the scan is no refusal, so it still answers as broken.
func TestPolicyRemoveFailsWhenTheRecordsCannotBeListed(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	repo := &fakeRepo{r: &recorder{}, listErr: errors.Join(unreadable("broken-1"), errors.New("read sandboxes: permission denied"))}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies})

	err := stores.RemovePolicy("web")
	if err == nil {
		t.Fatal("RemovePolicy removed the policy over records it could not list")
	}
	if held, ok := errors.AsType[*sandbox.HeldError](err); ok {
		t.Errorf("RemovePolicy refused as held by %v, when the scan itself failed", held.Users)
	}
}

// A record that does not read back may grant the secret, so rm without --force refuses and names it (SHARD-584).
func TestSecretRemoveRefusesWhileARecordDoesNotReadBack(t *testing.T) {
	secrets, err := secret.New(filepath.Join(t.TempDir(), "secrets"), nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{r: &recorder{}, left: []models.Sandbox{{ID: "sb-1", Secrets: []string{"TOKEN"}}}, listErr: unreadable("broken-1")}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Secrets: secrets})
	if _, err := stores.SetSecret("TOKEN", sandbox.SecretRequest{Value: "synthetic-value", Destinations: []string{"a.example.com"}}); err != nil {
		t.Fatal(err)
	}

	err = stores.RemoveSecret("TOKEN", false)
	var held *sandbox.HeldError
	if !errors.As(err, &held) || !slices.Equal(held.Users, []string{"sb-1", "broken-1"}) {
		t.Fatalf("RemoveSecret = %v, want a refusal that names the grantee and the record it could not read", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal reads %q, which does not offer --force", err.Error())
	}
	if _, err := secrets.Get("TOKEN"); err != nil {
		t.Errorf("the refusal still removed the secret: %v", err)
	}
}

// The policy is stored by the time the holders are read, so the failure says so rather than leaving the
// operator to guess whether it landed.
func TestSetPolicySaysThePolicyLandedWhenItCannotTellWhoHoldsIt(t *testing.T) {
	policies := &fakePolicies{policy: models.Policy{Name: "web"}}
	repo := &fakeRepo{r: &recorder{fail: []string{"repo.List"}}}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: repo, Policies: policies, Compiler: fakeCompiler{}})

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

// A stored name that does not resolve would fail the creates of every other sandbox, so it is refused before the store (SHARD-276).
func TestSetPolicyRefusesANameThatDoesNotResolve(t *testing.T) {
	policies := &fakePolicies{}
	compiler := fakeCompiler{err: errors.New(`rule "allow gone.example.com": resolve gone.example.com: no such host`)}
	stores := sandbox.NewStores(sandbox.StoresConfig{Repo: &fakeRepo{r: &recorder{}}, Policies: policies, Compiler: compiler})

	_, err := stores.SetPolicy(t.Context(), "web", sandbox.PolicyRequest{Rules: []sandbox.RuleText{{Action: models.ActionAllow, Rule: "gone.example.com"}}})

	var refused *sandbox.RequestError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "policy web") || !strings.Contains(err.Error(), "gone.example.com") {
		t.Fatalf("SetPolicy = %v, want a refusal that names the policy and the host", err)
	}
	if policies.policy.Name != "" {
		t.Errorf("the policy %+v was stored", policies.policy)
	}
}

// A malformed name is the caller's mistake, so show and rm answer it as SetPolicy does, never as an internal failure (SHARD-517).
func TestPolicyShowAndPolicyRemoveRefuseAMalformedNameAsABadRequest(t *testing.T) {
	stores, policies := heldBy(t, nil)

	_, showErr := stores.Policy("Bad")
	removeErr := stores.RemovePolicy("Bad")

	for verb, err := range map[string]error{"Policy": showErr, "RemovePolicy": removeErr} {
		if _, ok := errors.AsType[*sandbox.RequestError](err); !ok {
			t.Errorf("%s(%q) = %v, want a RequestError", verb, "Bad", err)
		}
	}
	if policies.removed != "" {
		t.Errorf("the refusal still removed policy %q", policies.removed)
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
