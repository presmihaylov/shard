package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/services/sandbox"
)

// create asks the daemon for a sandbox and prints the id; the pull happens there, so the call has no bound.
func (a App) create(ctx context.Context, args []string) error {
	req, err := parseCreate(args)
	if err != nil {
		return err
	}

	sb, err := a.client().CreateSandbox(ctx, req)
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// parseCreate splits the flags, the image and the argv, and refuses a typo before the daemon is asked.
func parseCreate(args []string) (sandbox.CreateRequest, error) {
	var req sandbox.CreateRequest

	flags := flag.NewFlagSet("shard create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&req.Name, "name", "", "a handle every verb takes in place of the id")
	flags.Var((*envList)(&req.Env), "env", "an environment variable as KEY=VALUE, repeatable")
	flags.Var((*secretList)(&req.Secrets), "secret", "a stored secret the guest gets a placeholder for, repeatable")
	flags.StringVar(&req.Policy, "policy", "", "the egress policy the host enforces")
	flags.StringVar(&req.WorkDir, "workdir", "", "the directory the entrypoint starts in")
	flags.StringVar(&req.User, "user", "", "the user the entrypoint runs as")
	flags.Int64Var(&req.Resources.MemoryMiB, "memory", 0, "the memory bound in MiB, 0 for unbounded")
	flags.IntVar(&req.Resources.VCPUs, "cpus", 0, "the vcpu bound, 0 for unbounded")

	if err := flags.Parse(args); err != nil {
		return sandbox.CreateRequest{}, fmt.Errorf("parse the create flags: %w", err)
	}

	// The spelling is checked here, so a name no verb could take back never costs the operator a pull.
	if named(flags) {
		if err := sandbox.ValidName(req.Name); err != nil {
			return sandbox.CreateRequest{}, err
		}
	}

	// A bound below zero is not a spelling of unbounded, and the substrate would drop it without a word.
	if req.Resources.MemoryMiB < 0 {
		return sandbox.CreateRequest{}, fmt.Errorf("--memory is a bound in MiB and cannot be negative, got %d", req.Resources.MemoryMiB)
	}
	// A bound this large overflows the byte count it is turned into, and an overflow reads as unbounded.
	if req.Resources.MemoryMiB > sandbox.MaxMemoryMiB {
		return sandbox.CreateRequest{}, fmt.Errorf("--memory is a bound in MiB and no host holds that much, got %d", req.Resources.MemoryMiB)
	}
	if req.Resources.VCPUs < 0 {
		return sandbox.CreateRequest{}, fmt.Errorf("--cpus is a bound and cannot be negative, got %d", req.Resources.VCPUs)
	}

	if req.Policy != "" {
		if err := sandbox.ValidPolicyName(req.Policy); err != nil {
			return sandbox.CreateRequest{}, err
		}
	}

	// An --env of the same name would either hide the placeholder or be hidden by it, and either is a surprise.
	for _, entry := range req.Env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains(req.Secrets, key) {
			return sandbox.CreateRequest{}, fmt.Errorf("--secret %s and --env %s name the same variable: the guest gets the placeholder as $%s, so drop the --env", key, key, key)
		}
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return sandbox.CreateRequest{}, errors.New("create takes one image reference, got none")
	}

	req.Image, rest = rest[0], rest[1:]
	if len(rest) == 0 {
		return req, nil
	}

	if rest[0] != "--" {
		return sandbox.CreateRequest{}, fmt.Errorf("unexpected argument %q: the flags go before the image and the command after --", rest[0])
	}

	req.Command = rest[1:]
	if len(req.Command) == 0 {
		return sandbox.CreateRequest{}, errors.New("-- takes the command to run, and nothing followed it")
	}

	return req, nil
}

// named says --name was given, so an explicit empty one is refused rather than read as no name.
func named(flags *flag.FlagSet) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "name" {
			set = true
		}
	})

	return set
}

// envList refuses anything that is not an assignment, because a merge drops such an entry and
// create would then report success with the variable absent.
type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }

func (e *envList) Set(value string) error {
	key, _, found := strings.Cut(value, "=")
	if !found {
		return fmt.Errorf("%q is not KEY=VALUE", value)
	}
	if key == "" {
		return fmt.Errorf("%q has no name", value)
	}

	*e = append(*e, value)

	return nil
}

// secretList refuses a name the store could not hold, so a typo is caught before anything is pulled.
type secretList []string

func (s *secretList) String() string { return strings.Join(*s, ",") }

func (s *secretList) Set(value string) error {
	if err := sandbox.ValidSecretName(value); err != nil {
		return err
	}
	if slices.Contains(*s, value) {
		return fmt.Errorf("--secret %s was given twice", value)
	}

	*s = append(*s, value)

	return nil
}
