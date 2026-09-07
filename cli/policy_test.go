package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
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
	app, _ := newClientApp(t, &out, sb)

	if err := app.Run(t.Context(), []string{"policy", "create", "--deny", "any", "web"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()

	if err := app.Run(t.Context(), []string{"inspect", "web"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}

	var got sandbox.Inspection
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("inspect printed something that is not JSON: %v\n%s", err, out.String())
	}
	if got.ID != "sandbox1" || got.Egress == nil || got.Egress.Policy != "web" || len(got.Egress.Rules) != 1 {
		t.Errorf("inspect printed %s", out.String())
	}
}

func TestPolicyShowNamesTheSandboxesThatHoldThePolicy(t *testing.T) {
	var out bytes.Buffer

	app, d := newLifecycleApp(t, &out, &recorder{}, stopped())
	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "api.example.com", "web"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "show", "web"}); err != nil {
		t.Fatalf("policy show: %v", err)
	}
	if strings.Contains(out.String(), "holders") {
		t.Errorf("policy show printed holders for a policy no sandbox holds: %s", out.String())
	}

	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sb-1", Policy: "web"}, {ID: "sb-2"}}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "show", "web"}); err != nil {
		t.Fatalf("policy show: %v", err)
	}

	var shown sandbox.PolicyView
	if err := json.Unmarshal(out.Bytes(), &shown); err != nil {
		t.Fatalf("decode %s: %v", out.String(), err)
	}
	if !slices.Equal(shown.Holders, []string{"sb-1"}) {
		t.Errorf("policy show printed holders %v, want the one record that names it", shown.Holders)
	}
	if len(shown.Rules) != 1 {
		t.Errorf("policy show printed %d rules beside the holders", len(shown.Rules))
	}
}

func TestPolicyAttachAndDetachRoundTrip(t *testing.T) {
	var out bytes.Buffer

	app, repo, _ := grantApp(t, &out, models.StateStopped)

	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "any", "locked"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "attach", "web", "locked"}); err != nil {
		t.Fatalf("policy attach: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("attach printed %q, want the sandbox id", got)
	}
	if repo.sb.Policy != "locked" {
		t.Errorf("the record holds %q, want the policy", repo.sb.Policy)
	}

	// policy rm and the POLICY column read the record, so both follow the attach without a change of their own.
	repo.left = []models.Sandbox{repo.sb}
	err := app.Run(t.Context(), []string{"policy", "rm", "locked"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("policy rm of an attached policy = %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"ls", "--all"}); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out.String(), "locked") {
		t.Errorf("ls printed %q, want the policy", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "detach", "web"}); err != nil {
		t.Fatalf("policy detach: %v", err)
	}
	if repo.sb.Policy != "" {
		t.Errorf("the record still holds %q", repo.sb.Policy)
	}
}

func TestPolicyAttachRefusesARunningSandboxAndAMissingPolicy(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := grantApp(t, &out, models.StateRunning)
	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "any", "locked"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}

	err := app.Run(t.Context(), []string{"policy", "attach", "web", "locked"})
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("the attach of a running sandbox = %v", err)
	}

	app, repo, _ := grantApp(t, &out, models.StateStopped)

	err = app.Run(t.Context(), []string{"policy", "attach", "web", "missing"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("the attach of a policy the store does not hold = %v", err)
	}
	if repo.sb.Policy != "" {
		t.Errorf("the refused attach wrote %q to the record", repo.sb.Policy)
	}
}

func TestParsePolicyAttachRefusesTheWrongArguments(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := grantApp(t, &out, models.StateStopped)

	for _, args := range [][]string{
		{"policy", "attach", "web"},
		{"policy", "attach", "web", "locked", "other"},
		{"policy", "attach", "--force", "web", "locked"},
		{"policy", "detach"},
		{"policy", "detach", "web", "locked"},
		{"policy", "detach", "--force", "web"},
	} {
		if err := app.Run(t.Context(), args); err == nil {
			t.Errorf("%v was taken", args)
		}
	}
}

// A grant opens nothing, so inspect prints the policy's own rules and never one implied by a secret.
func TestInspectShowsNoRuleImpliedByAGrant(t *testing.T) {
	var out bytes.Buffer

	app, _, _ := grantApp(t, &out, models.StateStopped)

	if err := app.Run(t.Context(), []string{"policy", "create", "--deny", "any", "locked"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}
	if err := app.Run(t.Context(), []string{"secret", "grant", "web", "TOKEN"}); err != nil {
		t.Fatalf("secret grant: %v", err)
	}
	if err := app.Run(t.Context(), []string{"policy", "attach", "web", "locked"}); err != nil {
		t.Fatalf("policy attach: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"inspect", "web"}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if strings.Contains(out.String(), "implied") || strings.Contains(out.String(), "api.example.com") {
		t.Errorf("inspect printed a rule the grant implied:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"policy": "locked"`) {
		t.Errorf("inspect does not name the policy:\n%s", out.String())
	}
}

// The lookup fails an hour later inside the guest, so create says it while the operator can still act.
func TestPolicyCreateNotesAPolicyThatOpensNoDNS(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule string
		note bool
	}{
		{"addresses", "203.0.113.7 tcp:443", true},
		{"named", "api.example.com", false},
		{"asked", "dns", false},
	} {
		var out bytes.Buffer
		app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

		if err := app.Run(t.Context(), []string{"policy", "create", "--allow", tc.rule, tc.name}); err != nil {
			t.Fatalf("policy create %s: %v", tc.name, err)
		}
		if got := strings.Contains(out.String(), noteNoDNS); got != tc.note {
			t.Errorf("allow %s printed %q, want the note to be %v", tc.rule, out.String(), tc.note)
		}
	}
}
