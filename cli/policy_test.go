package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestPolicyCreateStoresTheRulesInOrder(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newLifecycleApp(t, &out, r, stopped())

	err := app.Run(t.Context(), []string{"policy", "create", "--deny", "cidr:10.0.0.0/8", "--allow", "domain:api.example.com", "--deny", "group:any", "web"})
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

	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "group:any", "web"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}
	if !slices.Contains(r.calls, "net.ReapplyAll") {
		t.Errorf("the new rules did not reach the host: %v", r.calls)
	}

	// The store holds the policy, but the host still enforces the old rules: the operator must know.
	r.fail = []string{"net.ReapplyAll"}
	err := app.Run(t.Context(), []string{"policy", "create", "--deny", "group:any", "web"})
	if err == nil || !strings.Contains(err.Error(), "still enforces") {
		t.Errorf("policy create = %v, want a warning that the host is behind", err)
	}
}

func TestPolicyCreateRefusesWhatTheHostCannotEnforce(t *testing.T) {
	app, _ := newLifecycleApp(t, &bytes.Buffer{}, &recorder{}, stopped())

	for _, args := range [][]string{
		{"policy", "create", "--allow", "domain-suffix:example.com", "web"},
		{"policy", "create", "--allow", "domain:api.example.com tcp:22", "web"},
		{"policy", "create", "--allow", "group:any", "Web"},
		{"policy", "create", "web", "--allow", "group:any"},
		{"policy", "create"},
	} {
		if err := app.Run(t.Context(), args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}

	err := app.Run(t.Context(), []string{"policy", "create", "--allow", "domain-suffix:example.com", "web"})
	if err == nil || !strings.Contains(err.Error(), "SHARD-71") {
		t.Errorf("a domain-suffix rule got %v, want a refusal that names the proxy ticket", err)
	}
}

func TestPolicyApplyReadsAFile(t *testing.T) {
	var out bytes.Buffer

	app, d := newLifecycleApp(t, &out, &recorder{}, stopped())

	file := filepath.Join(t.TempDir(), "web.json")
	blob := `{"name":"web","rules":[{"action":"allow","destination":{"kind":"domain","value":"api.example.com"},"protocol":"tcp","ports":[443]}]}`
	if err := os.WriteFile(file, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := app.Run(t.Context(), []string{"policy", "apply", "-f", file}); err != nil {
		t.Fatalf("policy apply: %v", err)
	}

	policies, err := d.policies()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := policies.Get("web"); err != nil || len(got.Rules) != 1 || !slices.Equal(got.Rules[0].Ports, []int{443}) {
		t.Errorf("Get = %+v, %v", got, err)
	}

	if err := os.WriteFile(file, []byte(`{"name":"web","rules":[{"action":"allow","destination":{"kind":"domain-suffix","value":"example.com"},"protocol":"tcp"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context(), []string{"policy", "apply", "-f", file}); err == nil {
		t.Error("a file with a domain-suffix rule was applied")
	}
	if err := app.Run(t.Context(), []string{"policy", "apply"}); err == nil {
		t.Error("apply without -f was accepted")
	}

	// A misspelled field would otherwise widen the rule in silence.
	if err := os.WriteFile(file, []byte(`{"name":"web","rules":[{"action":"allow","destination":{"kind":"cidr","value":"1.1.1.1"},"protocol":"tcp","port":[443]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context(), []string{"policy", "apply", "-f", file}); err == nil {
		t.Error("a file with an unknown field was applied")
	}
}

func TestPolicyRemoveRefusesWhileASandboxHoldsIt(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newLifecycleApp(t, &out, r, stopped())
	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sandbox1", Policy: "web"}, {ID: "sandbox2"}}

	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "group:any", "web"}); err != nil {
		t.Fatal(err)
	}

	err := app.Run(t.Context(), []string{"policy", "rm", "web"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || strings.Contains(err.Error(), "sandbox2") {
		t.Errorf("policy rm = %v, want a refusal that names sandbox1 only", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"policy", "rm", "--force", "web"}); err != nil {
		t.Fatalf("policy rm --force: %v", err)
	}
	if !strings.Contains(out.String(), "warning") || !strings.Contains(out.String(), "sandbox1") {
		t.Errorf("rm --force printed %q, want a warning that names the sandbox left with nothing", out.String())
	}
	if !slices.Contains(r.calls, "net.ReapplyAll") {
		t.Errorf("the host was not told the policy is gone: %v", r.calls)
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

	if err := app.Run(t.Context(), []string{"policy", "create", "--deny", "group:any", "web"}); err != nil {
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
