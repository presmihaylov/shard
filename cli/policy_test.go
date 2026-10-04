package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/egress"
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
	if slices.Contains(r.seen(), "net.ReapplyAll") {
		t.Errorf("a policy no sandbox holds was applied: %v", r.seen())
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
	if err := app.Run(t.Context(), []string{"policy", "list"}); err != nil {
		t.Fatalf("policy list: %v", err)
	}
	if !strings.Contains(out.String(), "NAME") || !strings.Contains(out.String(), "web") {
		t.Errorf("policy list printed %q", out.String())
	}
}

func TestPolicyCreateEnforcesAtOnceOnTheSandboxesThatHoldIt(t *testing.T) {
	r := &recorder{}
	app, d := newLifecycleApp(t, &bytes.Buffer{}, r, stopped())
	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sandbox1", Policy: "web"}}

	if err := app.Run(t.Context(), []string{"policy", "create", "--allow", "any", "web"}); err != nil {
		t.Fatalf("policy create: %v", err)
	}
	if !slices.Contains(r.seen(), "net.ReapplyAll") {
		t.Errorf("the new rules did not reach the host: %v", r.seen())
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
		{"policy", "create", "--allow", "gone.invalid", "web"},
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

	err := app.Run(t.Context(), []string{"policy", "remove", "web"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") || strings.Contains(err.Error(), "sandbox2") {
		t.Errorf("policy remove = %v, want a refusal that names sandbox1 only", err)
	}

	if err := app.Run(t.Context(), []string{"policy", "remove", "--force", "web"}); err == nil {
		t.Error("policy remove --force accepted, want a refusal: the flag is gone")
	}

	d.repoSvc.(*fakeLifecycleRepo).left = []models.Sandbox{{ID: "sandbox2"}}
	r.forget()
	if err := app.Run(t.Context(), []string{"policy", "remove", "web"}); err != nil {
		t.Fatalf("policy remove with no holder: %v", err)
	}
	if slices.Contains(r.seen(), "net.ReapplyAll") {
		t.Errorf("remove of an unheld policy touched the host: %v", r.seen())
	}

	if err := app.Run(t.Context(), []string{"policy", "remove", "web"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("remove of a missing policy = %v", err)
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

	var got client.Inspection
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

	// policy remove and the POLICY column read the record, so both follow the attach without a change of their own.
	repo.left = []models.Sandbox{repo.sb}
	err := app.Run(t.Context(), []string{"policy", "remove", "locked"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("policy remove of an attached policy = %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "locked") {
		t.Errorf("list printed %q, want the policy", out.String())
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
		name  string
		rules []string
		note  bool
	}{
		{"addresses", []string{"--allow", "203.0.113.7 tcp:443"}, true},
		{"named", []string{"--allow", "api.example.com"}, false},
		{"asked", []string{"--allow", "dns"}, false},
		// An allow any reaches the resolver with no implied rule, so the guest resolves (SHARD-315).
		{"any", []string{"--allow", "any"}, false},
		{"web", []string{"--allow", "any tcp:443"}, true},
		{"denied", []string{"--allow", "1.0.0.1", "--deny", "any"}, true},
		// The resolver takes the first match, so a deny ahead of the allow can refuse every name.
		{"reversed", []string{"--deny", "any", "--allow", "any"}, true},
		{"shadowed", []string{"--deny", "any", "--allow", "api.example.com"}, true},
		{"narrow", []string{"--deny", "api.example.com", "--allow", "api.example.com"}, true},
	} {
		var out bytes.Buffer
		app, _ := newLifecycleApp(t, &out, &recorder{}, stopped())

		args := append(append([]string{"policy", "create"}, tc.rules...), tc.name)
		if err := app.Run(t.Context(), args); err != nil {
			t.Fatalf("policy create %s: %v", tc.name, err)
		}
		if got := strings.Contains(out.String(), noteNoDNS); got != tc.note {
			t.Errorf("%v printed %q, want the note to be %v", tc.rules, out.String(), tc.note)
		}
	}
}

func TestParsePolicyLogsTakesBothFollowSpellings(t *testing.T) {
	for _, args := range [][]string{{"-f", "sandbox1"}, {"--follow", "sandbox1"}} {
		opts, err := parseLogs("policy logs", args)
		if err != nil {
			t.Fatalf("parseLogs(%q): %v", args, err)
		}
		if opts.id != "sandbox1" || !opts.follow {
			t.Errorf("parseLogs(%q) gave %+v, want sandbox1 and a follow", args, opts)
		}
	}

	opts, err := parseLogs("policy logs", []string{"sandbox1"})
	if err != nil {
		t.Fatalf("parseLogs: %v", err)
	}
	if opts.id != "sandbox1" || opts.follow {
		t.Errorf("parseLogs gave %+v, want sandbox1 and no follow", opts)
	}
}

func TestParsePolicyLogsRefusesTheWrongArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no sandbox":       {nil, "takes one sandbox id or name, got none"},
		"two sandboxes":    {[]string{"sandbox1", "sandbox2"}, `takes one sandbox id or name, got ["sandbox1" "sandbox2"]`},
		"a flag after it":  {[]string{"sandbox1", "-f"}, `takes one sandbox id or name, got ["sandbox1" "-f"]`},
		"an unknown flag":  {[]string{"--egress", "sandbox1"}, "unknown flag --egress"},
		"a policy's flags": {[]string{"--allow", "dns", "sandbox1"}, "unknown flag --allow"},
	} {
		_, err := parseLogs("policy logs", tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseLogs(%s) returned %v, want %q", name, err, tc.want)
		}
	}
}

// decisions is a proxy allow and a host drop, the two sources a reader tells apart by the source field.
func decisions() []egress.Record {
	return []egress.Record{
		{Time: time.Unix(1, 0).UTC(), Source: egress.SourceProxy, Verdict: "allow", Host: "api.example.com", Port: 443, Rule: "1"},
		{Time: time.Unix(2, 0).UTC(), Source: egress.SourceHost, Verdict: "deny", Address: "203.0.113.7", Port: 25, Rule: "default"},
	}
}

func checkDecisionLines(t *testing.T, printed string) {
	t.Helper()

	lines := strings.Split(strings.TrimRight(printed, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("policy logs printed %q, want two lines", printed)
	}

	var first egress.Record
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode the first line: %v", err)
	}
	if first.Host != "api.example.com" || first.Rule != "1" {
		t.Errorf("the first line is %+v", first)
	}
	if !strings.Contains(lines[1], `"source":"host"`) {
		t.Errorf("the second line is %s", lines[1])
	}
}

func TestPolicyLogsPrintsTheDecisionsByIDAndByName(t *testing.T) {
	for _, ref := range []string{"sandbox1", "web"} {
		var out bytes.Buffer

		sb := running()
		sb.Name = "web"
		app, d := newClientApp(t, &out, sb)
		d.egressLog = decisions()

		if err := app.Run(t.Context(), []string{"policy", "logs", ref}); err != nil {
			t.Fatalf("policy logs %s: %v", ref, err)
		}
		checkDecisionLines(t, out.String())
	}
}

// A follow prints what the log holds, then says on stderr why it ended when the sandbox goes.
func TestPolicyLogsFollowPrintsTheDecisionsUntilTheSandboxGoes(t *testing.T) {
	for _, flag := range []string{"-f", "--follow"} {
		var out, errOut bytes.Buffer

		app, d := newClientApp(t, &out, running())
		app.Err = &errOut
		d.egressLog = decisions()

		if err := app.Run(t.Context(), []string{"policy", "logs", flag, "sandbox1"}); err != nil {
			t.Fatalf("policy logs %s: %v", flag, err)
		}
		checkDecisionLines(t, out.String())
		if !strings.Contains(errOut.String(), "the egress log of sandbox sandbox1 ended") {
			t.Errorf("policy logs %s said %q on stderr, want why the follow ended", flag, errOut.String())
		}
	}
}

func TestPolicyLogsRefusesASandboxThatNeverExisted(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"policy", "logs", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("policy logs returned %v, want the id named", err)
	}
	if out.Len() != 0 {
		t.Errorf("policy logs printed %q", out.String())
	}
}

// A sandbox with no policy has made no decision, so the answer is an empty log and no error.
func TestPolicyLogsPrintsNothingForASandboxWithNoPolicy(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"policy", "logs", "sandbox1"}); err != nil {
		t.Fatalf("policy logs: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("policy logs printed %q, want nothing", out.String())
	}
}
