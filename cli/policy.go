package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
)

// maxPolicyBytes bounds what apply reads, so a stray redirect does not become a policy.
const maxPolicyBytes = 1 << 20

func (a App) policy(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("policy takes a subcommand: create, apply, show, ls or rm")
	}

	switch args[0] {
	case "create":
		return a.policyCreate(ctx, args[1:])
	case "apply":
		return a.policyApply(ctx, args[1:])
	case "show":
		return a.policyShow(args[1:])
	case "ls", "list":
		return a.policyList(args[1:])
	case "rm", "remove":
		return a.policyRemove(ctx, args[1:])
	}

	return fmt.Errorf("unknown policy subcommand %q; run shard help", args[0])
}

// ruleList collects --allow and --deny into one slice, so the order on the command line is the order
// the host evaluates them in.
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

func (a App) policyApply(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("shard policy apply", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("f", "", "the policy as JSON, or - for stdin")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse the policy apply flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("policy apply takes no arguments beyond -f, got %d", flags.NArg())
	}
	if *file == "" {
		return errors.New("policy apply needs -f <file>, the policy as JSON, or -f - for stdin")
	}

	d := a.deps()

	blob, err := readPolicy(d, *file)
	if err != nil {
		return err
	}

	var policy models.Policy
	if err := json.Unmarshal(blob, &policy); err != nil {
		return fmt.Errorf("decode the policy in %s: %w", *file, err)
	}

	return a.storePolicy(ctx, policy)
}

func readPolicy(d *deps, file string) ([]byte, error) {
	var in io.Reader = d.stdin()
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("read the policy: %w", err)
		}
		defer f.Close()
		in = f
	}

	blob, err := io.ReadAll(io.LimitReader(in, maxPolicyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the policy from %s: %w", file, err)
	}
	if len(blob) > maxPolicyBytes {
		return nil, fmt.Errorf("the policy in %s is larger than %d bytes", file, maxPolicyBytes)
	}

	return blob, nil
}

// storePolicy writes the policy and, when a sandbox holds it, puts the new rules on the host at once:
// a policy change is never left for the next start.
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

	policies, err := a.deps().policies()
	if err != nil {
		return err
	}

	policy, err := policies.Get(args[0])
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(policy, "", "  ")
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

type policyRemoveOptions struct {
	name  string
	force bool
}

func (a App) policyRemove(ctx context.Context, args []string) error {
	opts, err := parsePolicyRemove(args)
	if err != nil {
		return err
	}

	d := a.deps()

	policies, err := d.policies()
	if err != nil {
		return err
	}

	if _, err := policies.Get(opts.name); err != nil {
		return err
	}

	users, err := policyUsers(d, opts.name)
	if err != nil {
		return err
	}
	if len(users) != 0 && !opts.force {
		return fmt.Errorf("policy %s is held by sandbox %s: remove the sandbox first, or pass --force", opts.name, strings.Join(users, ", "))
	}

	if err := policies.Remove(opts.name); err != nil {
		return err
	}

	// The record still names the policy, and a policy that does not exist drops everything: fail closed.
	if len(users) != 0 {
		a.warn(fmt.Sprintf("sandbox %s still names policy %s and now reaches nothing", strings.Join(users, ", "), opts.name))
		if err := reapplyAll(ctx, d); err != nil {
			return fmt.Errorf("policy %s is removed, but the host still enforces the rules it had: %w", opts.name, err)
		}
	}

	return a.print(opts.name)
}

func parsePolicyRemove(args []string) (policyRemoveOptions, error) {
	var opts policyRemoveOptions

	flags := flag.NewFlagSet("shard policy rm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.force, "force", false, "remove the policy even when a sandbox holds it")

	if err := flags.Parse(args); err != nil {
		return policyRemoveOptions{}, fmt.Errorf("parse the policy rm flags: %w", err)
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return policyRemoveOptions{}, errors.New("policy rm takes its flags before the name: shard policy rm --force <name>")
	}
	if len(rest) != 1 {
		return policyRemoveOptions{}, fmt.Errorf("policy rm takes one name, got %d", len(rest))
	}

	opts.name = rest[0]

	return opts, egress.ValidName(opts.name)
}
