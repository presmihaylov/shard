package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
)

// ruleList collects --allow and --deny into one slice, in the order the host evaluates them.
type ruleList struct {
	action models.Action
	rules  *[]client.RuleText
}

func (l ruleList) String() string { return "" }

// Set keeps the rule as it was typed: the daemon owns the grammar, so the CLI never parses one.
func (l ruleList) Set(text string) error {
	*l.rules = append(*l.rules, client.RuleText{Action: l.action, Rule: text})

	return nil
}

func (a App) policyCreate(ctx context.Context, args []string) error {
	name, rules, err := parsePolicyCreate(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	policy, err := c.SetPolicy(ctx, name, rules)
	if err != nil {
		return err
	}

	// The lookup fails inside the guest an hour later, so say it now, while the operator can act.
	if policy.DNS == dnsClosed && a.Err != nil {
		fmt.Fprintln(a.Err, noteNoDNS)
	}

	return a.print(policy.Name)
}

const (
	dnsClosed = "closed"
	noteNoDNS = "note: this policy opens no DNS; a program that looks a name up fails on the lookup; add a name rule, or --allow dns"
)

func parsePolicyCreate(args []string) (string, []client.RuleText, error) {
	var rules []client.RuleText

	flags := newFlags("policy create")
	flags.Var(ruleList{action: models.ActionAllow, rules: &rules}, "allow", "")
	flags.Var(ruleList{action: models.ActionDeny, rules: &rules}, "deny", "")

	if err := parseVerb(flags, args); err != nil {
		return "", nil, err
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return "", nil, errors.New("policy create takes its flags before the name: shard policy create --allow <rule> <name>")
	}
	if len(rest) != 1 {
		return "", nil, fmt.Errorf("policy create takes one name, got %d", len(rest))
	}

	return rest[0], rules, nil
}

func (a App) policyShow(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("policy show", args, formatJSON)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("policy show takes one name, got %d", len(rest))
	}
	if err := formatLanded("policy show", format, formatJSON); err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	policy, err := c.GetPolicy(ctx, rest[0])
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return fmt.Errorf("encode policy %s: %w", policy.Name, err)
	}

	return a.print(string(blob))
}

func (a App) policyList(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("policy list", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("policy list takes no arguments, got %d", len(rest))
	}
	if err := formatLanded("policy list", format, formatTable); err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	all, err := c.ListPolicies(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tRULES")

	for _, policy := range all {
		fmt.Fprintf(w, "%s\t%d\n", policy.Name, len(policy.Rules))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}

func (a App) policyRemove(ctx context.Context, args []string) error {
	name, err := parsePolicyRemove(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	if err := c.RemovePolicy(ctx, name); err != nil {
		return err
	}

	return a.print(name)
}

func parsePolicyRemove(args []string) (string, error) {
	rest, err := parseArgs("policy remove", args)
	if err != nil {
		return "", err
	}
	if len(rest) != 1 {
		return "", fmt.Errorf("policy remove takes one name, got %d", len(rest))
	}

	return rest[0], nil
}

// policyAttach hands a sandbox that already exists the policy a create with --policy would have given it.
func (a App) policyAttach(ctx context.Context, args []string) error {
	rest, err := policyArgs("attach", args, 2, "shard policy attach <id|name> <policy>")
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.AttachPolicy(ctx, rest[0], rest[1])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

func (a App) policyDetach(ctx context.Context, args []string) error {
	rest, err := policyArgs("detach", args, 1, "shard policy detach <id|name>")
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.DetachPolicy(ctx, rest[0])
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// policyArgs parses a policy verb that takes no flags, and refuses any count but the one it wants.
func policyArgs(verb string, args []string, want int, usage string) ([]string, error) {
	rest, err := parseArgs("policy "+verb, args)
	if err != nil {
		return nil, err
	}
	// A sandbox name may start with -, after a --, but nothing after it does: one that seems to is a misplaced flag.
	if len(rest) > 1 && slices.ContainsFunc(rest[1:], func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return nil, fmt.Errorf("policy %s takes no flags: %s", verb, usage)
	}
	if len(rest) != want {
		return nil, fmt.Errorf("policy %s takes %d arguments, got %d: %s", verb, want, len(rest), usage)
	}

	return rest, nil
}
