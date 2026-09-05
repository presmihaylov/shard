package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestPolicyCreateStoresTheRulesInOrder(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newLifecycleApp(t, &out, r, stopped())

	err := app.Run(t.Context(), []string{"policy", "create", "--deny", "10.0.0.0/8", "--allow", "api.example.com", "--deny", "any", "web"})
	if err != nil {
		t.Fatalf("policy create: %v", err)
	}
	if out.String() != "web\n" {
		t.Errorf("policy create printed %q, want the name", out.String())
	}
	if slices.Contains(r.calls, "net.ReapplyAll") {
		t.Errorf("a policy no sandbox holds was applied: %v", r.calls)
	}

	policies, err := d.policies()
	if err != nil {
		t.Fatal(err)
	}
	got, err := policies.Get("web")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var shape []string
	for _, rule := range got.Rules {
		shape = append(shape, string(rule.Action)+" "+rule.Destination.Value)
	}
	if want := []string{"deny 10.0.0.0/8", "allow api.example.com", "deny any"}; !slices.Equal(shape, want) {
		t.Errorf("the policy holds %v, want %v", shape, want)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "show", "web"}); err != nil {
		t.Fatalf("policy show: %v", err)
	}
	var shown models.Policy
	if err := json.Unmarshal(out.Bytes(), &shown); err != nil || shown.Name != "web" || len(shown.Rules) != 3 {
		t.Errorf("policy show printed %s (%v)", out.String(), err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "ls"}); err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	if !strings.Contains(out.String(), "NAME") || !strings.Contains(out.String(), "web") {
		t.Errorf("policy ls printed %q", out.String())
	}
}

func TestPolicyCreateEnforcesAtOnceOnTheSandboxesThatHoldIt(t *testing.T) {
	r := &recorder{}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, stopped())
	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sandbox1", Policy: "web"}}

	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "any", "web"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}
	if !slices.Contains(r.calls, "net.ReapplyAll") {
		t.Errorf("the new rules did not reach the host: %v", r.calls)
	}

	// The store holds the policy, but the host still enforces the old rules: the operator must know.
	r.fail = []string{"net.ReapplyAll"}
	err := app.Run(t.Context(), []string{"policy", "create", "--deny", "any", "web"})
	if err == nil || !strings.Contains(err.Error(), "still enforces") {
		t.Errorf("policy create = %v, want a warning that the host is behind", err)
	}
}

func TestPolicyCreateRefusesWhatTheHostCannotEnforce(t *testing.T) {
	app, _ := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, stopped())

	for _, args := range [][]string{
		{"policy", "create", "--allow", "suffix:example.com tcp:22", "web"},
		{"policy", "create", "--allow", "api.example.com tcp:22", "web"},
		{"policy", "create", "--allow", "any", "Web"},
		{"policy", "create", "web", "--allow", "any"},
		{"policy", "create"},
	} {
		if err := app.Run(t.Context(), args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}

	// A suffix and a wildcard are matched by name in the proxy, which is where every web request goes.
	for i, rule := range []string{"suffix:example.com", "*.example.com", "api.*.example.com tcp:443"} {
		if err := app.Run(t.Context(), []string{"policy", "create", "--allow", rule, fmt.Sprintf("web%d", i)}); err != nil {
			t.Errorf("a %s rule got %v, want the proxy to take it", rule, err)
		}
	}
}

func TestPolicyRemoveRefusesWhileASandboxHoldsIt(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newLifecycleApp(t, &out, r, stopped())
	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sandbox1", Policy: "web"}, {ID: "sandbox2"}}

	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "any", "web"}); err != nil {
		t.Fatal(err)
	}

	err := app.Run(t.Context(), []string{"policy", "rm", "web"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || strings.Contains(err.Error(), "sandbox2") {
		t.Errorf("policy rm = %v, want a refusal that names sandbox1 only", err)
	}

	if err := app.Run(t.Context(), []string{"policy", "rm", "--force", "web"}); err == nil {
		t.Error("policy rm --force accepted, want a refusal: the flag is gone")
	}

	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sandbox2"}}
	r.calls = nil
	if err := app.Run(t.Context(), []string{"policy", "rm", "web"}); err != nil {
		t.Fatalf("policy rm with no holder: %v", err)
	}
	if slices.Contains(r.calls, "net.ReapplyAll") {
		t.Errorf("rm of an unheld policy touched the host: %v", r.calls)
	}

	if err := app.Run(t.Context(), []string{"policy", "rm", "web"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("rm of a missing policy = %v", err)
	}
}

func TestInspectPrintsWhatTheHostEnforces(t *testing.T) {
	var out bytes.Buffer

	sb := stopped()
	sb.Policy = "web"
	app, _ := newLifecycleApp(t, &out, &recorder{}, sb)

	if err := app.Run(t.Context(), []string{"policy", "create", "--deny", "any", "web"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()

	if err := app.Run(t.Context(), []string{"inspect", "web"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	var got inspected
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("inspect printed something that is not JSON: %v\n%s", err, out.String())
	}
	if got.ID != "sandbox1" || got.Egress == nil || got.Egress.Policy != "web" || len(got.Egress.Rules) != 1 {
		t.Errorf("inspect printed %s", out.String())
	}
}
