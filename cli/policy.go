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
	"github.com/presmihaylov/shard/services/egress"
)

func (a App) policy(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("policy takes a subcommand: create, show, ls or rm")
	}

	switch args[0] {
	case "create":
		return a.policyCreate(ctx, args[1:])
	case "show":
		return a.policyShow(args[1:])
	case "ls", "list":
		return a.policyList(args[1:])
	case "rm", "remove":
		return a.policyRemove(args[1:])
	}

	return fmt.Errorf("unknown policy subcommand %q; run shard help", args[0])
}

// ruleList collects --allow and --deny into one slice, in the order the host evaluates them.
type ruleList struct {
	action models.Action
	rules  *[]models.Rule
}

func (l ruleList) String() string { return "" }

func (l ruleList) Set(text string) error {
	rule, err := egress.ParseRule(l.action, text)
	if err != nil {
		return err
	}
	*l.rules = append(*l.rules, rule)

	return nil
}

func (a App) policyCreate(ctx context.Context, args []string) error {
	policy, err := parsePolicyCreate(args)
	if err != nil {
		return err
	}

	return a.storePolicy(ctx, policy)
}

func parsePolicyCreate(args []string) (models.Policy, error) {
	var policy models.Policy

	flags := flag.NewFlagSet("shard policy create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(ruleList{action: models.ActionAllow, rules: &policy.Rules}, "allow", "a rule to allow, repeatable")
	flags.Var(ruleList{action: models.ActionDeny, rules: &policy.Rules}, "deny", "a rule to deny, repeatable")

	if err := flags.Parse(args); err != nil {
		return models.Policy{}, fmt.Errorf("parse the policy create flags: %w", err)
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return models.Policy{}, errors.New("policy create takes its flags before the name: shard policy create --allow <rule> <name>")
	}
	if len(rest) != 1 {
		return models.Policy{}, fmt.Errorf("policy create takes one name, got %d", len(rest))
	}

	policy.Name = rest[0]
	if err := egress.Validate(policy); err != nil {
		return models.Policy{}, err
	}

	return policy, nil
}

// storePolicy writes the policy and puts the new rules on every sandbox that holds it at once.
func (a App) storePolicy(ctx context.Context, policy models.Policy) error {
	d := a.deps()

	policies, err := d.policies()
	if err != nil {
		return err
	}

	if err := policies.Set(policy); err != nil {
		return err
	}

	users, err := policyUsers(d, policy.Name)
	if err != nil {
		return err
	}
	if len(users) != 0 {
		if err := reapplyAll(ctx, d); err != nil {
			return fmt.Errorf("policy %s is stored, but the host still enforces the rules it had: %w", policy.Name, err)
		}
	}

	return a.print(policy.Name)
}

func reapplyAll(ctx context.Context, d *deps) error {
	net, err := d.net()
	if err != nil {
		return err
	}

	return net.ReapplyAll(ctx)
}

// policyUsers names the sandboxes whose record holds the policy. A stopped one counts: start enforces it again.
func policyUsers(d *deps, name string) ([]string, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	sandboxes, unreadable := repo.List()
	if unreadable != nil {
		return nil, fmt.Errorf("cannot tell which sandboxes hold the policy: %w", unreadable)
	}

	var users []string
	for _, sb := range sandboxes {
		if sb.Policy == name {
			users = append(users, sb.ID)
		}
	}

	return users, nil
}

func (a App) policyShow(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("policy show takes one name, got %d", len(args))
	}

	d := a.deps()
	policies, err := d.policies()
	if err != nil {
		return err
	}

	policy, err := policies.Get(args[0])
	if err != nil {
		return err
	}

	holders, err := policyUsers(d, policy.Name)
	if err != nil {
		return err
	}

	shown := struct {
		models.Policy
		Holders []string `json:"holders,omitempty"`
	}{policy, holders}

	blob, err := json.MarshalIndent(shown, "", "  ")
	if err != nil {
		return fmt.Errorf("encode policy %s: %w", policy.Name, err)
	}

	return a.print(string(blob))
}

func (a App) policyList(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("policy ls takes no arguments, got %d", len(args))
	}

	policies, err := a.deps().policies()
	if err != nil {
		return err
	}

	all, err := policies.List()
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

func (a App) policyRemove(args []string) error {
	name, err := parsePolicyRemove(args)
	if err != nil {
		return err
	}

	d := a.deps()

	policies, err := d.policies()
	if err != nil {
		return err
	}

	if _, err := policies.Get(name); err != nil {
		return err
	}

	users, err := policyUsers(d, name)
	if err != nil {
		return err
	}
	if len(users) != 0 {
		return fmt.Errorf("policy %s is held by sandbox %s: remove the sandbox first", name, strings.Join(users, ", "))
	}

	if err := policies.Remove(name); err != nil {
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

	return args[0], egress.ValidName(args[0])
}
