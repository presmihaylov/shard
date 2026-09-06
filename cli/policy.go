package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
)

func (a App) policy(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("policy takes a subcommand: create, show, ls or rm")
	}

	switch args[0] {
	case "create":
		return a.policyCreate(ctx, args[1:])
	case "show":
		return a.policyShow(ctx, args[1:])
	case "ls", "list":
		return a.policyList(ctx, args[1:])
	case "rm", "remove":
		return a.policyRemove(ctx, args[1:])
	}

	return fmt.Errorf("unknown policy subcommand %q; run shard help", args[0])
}

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

	policy, err := a.client().SetPolicy(ctx, name, rules)
	if err != nil {
		return err
	}

	return a.print(policy.Name)
}

func parsePolicyCreate(args []string) (string, []client.RuleText, error) {
	var rules []client.RuleText

	flags := flag.NewFlagSet("shard policy create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(ruleList{action: models.ActionAllow, rules: &rules}, "allow", "a rule to allow, repeatable")
	flags.Var(ruleList{action: models.ActionDeny, rules: &rules}, "deny", "a rule to deny, repeatable")

	if err := flags.Parse(args); err != nil {
		return "", nil, fmt.Errorf("parse the policy create flags: %w", err)
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
	if len(args) != 1 {
		return fmt.Errorf("policy show takes one name, got %d", len(args))
	}

	policy, err := a.client().GetPolicy(ctx, args[0])
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
	if len(args) != 0 {
		return fmt.Errorf("policy ls takes no arguments, got %d", len(args))
	}

	all, err := a.client().ListPolicies(ctx)
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

	if err := a.client().RemovePolicy(ctx, name); err != nil {
		return err
	}

	return a.print(name)
}

func parsePolicyRemove(args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("policy rm takes one name, got %d", len(args))
	}
	if strings.HasPrefix(args[0], "-") {
		return "", errors.New("policy rm takes no flags: shard policy rm <name>")
	}

	return args[0], nil
}
