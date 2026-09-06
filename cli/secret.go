package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/pkg/pty"
)

// maxSecretBytes bounds what set reads, so a stray redirect of a disk image does not become a secret.
const maxSecretBytes = 64 << 10

func (a App) secret(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("secret takes a subcommand: set, ls or rm")
	}

	switch args[0] {
	case "set":
		return a.secretSet(ctx, args[1:])
	case "ls", "list":
		return a.secretList(ctx, args[1:])
	case "rm", "remove":
		return a.secretRemove(ctx, args[1:])
	}

	return fmt.Errorf("unknown secret subcommand %q; want set, ls or rm", args[0])
}

// secretSetOptions is one parsed shard secret set invocation.
type secretSetOptions struct {
	name         string
	destinations []string
	mock         string
}

// secretSet reads the value from stdin, so it lands in no shell history and no process listing.
func (a App) secretSet(ctx context.Context, args []string) error {
	opts, err := parseSecretSet(args)
	if err != nil {
		return err
	}

	if pty.IsTerminal(a.stdin()) {
		return errors.New("secret set reads the value from stdin: pipe it in, as in printf '%s' \"$TOKEN\" | shard secret set --to <host> NAME")
	}

	value, err := readSecretValue(a.stdin())
	if err != nil {
		return err
	}

	sec, err := a.client().SetSecret(ctx, opts.name, value, opts.destinations, opts.mock)
	if err != nil {
		return err
	}

	return a.print(sec.Name)
}

// readSecretValue takes the whole of stdin less one trailing newline, which is what echo and a
// heredoc add and no secret carries. A second newline stays: it is then part of the value.
func readSecretValue(in io.Reader) (string, error) {
	blob, err := io.ReadAll(io.LimitReader(in, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read the secret value from stdin: %w", err)
	}
	if len(blob) > maxSecretBytes {
		return "", fmt.Errorf("the secret value is longer than %d bytes, and no credential is", maxSecretBytes)
	}

	value := strings.TrimSuffix(string(blob), "\n")
	if value == "" {
		return "", errors.New("the secret value is empty: pipe it into stdin, as in printf '%s' \"$TOKEN\" | shard secret set --to <host> NAME")
	}

	return value, nil
}

func parseSecretSet(args []string) (secretSetOptions, error) {
	var opts secretSetOptions

	flags := flag.NewFlagSet("shard secret set", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var((*hostList)(&opts.destinations), "to", "a host the value may go to, repeatable")
	flags.StringVar(&opts.mock, "mock-value", "", "the placeholder the guest sees in place of the value")

	if err := flags.Parse(args); err != nil {
		return secretSetOptions{}, fmt.Errorf("parse the secret set flags: %w", err)
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return secretSetOptions{}, errors.New("secret set takes its flags before the name: shard secret set --to <host> <NAME>")
	}
	if len(rest) != 1 {
		return secretSetOptions{}, fmt.Errorf("secret set takes one name, got %d", len(rest))
	}

	opts.name = rest[0]

	return opts, nil
}

func (a App) secretList(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("secret ls takes no arguments, got %d", len(args))
	}

	result, err := a.client().ListSecrets(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESTINATIONS\tPLACEHOLDER\tUPDATED")

	for _, sec := range result.Secrets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sec.Name, strings.Join(sec.Destinations, ","), sec.MockValue, humanAge(sec.UpdatedAt))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	// The readable secrets are listed before the error, so one broken file does not hide the rest.
	if len(result.Warnings) != 0 {
		return errors.New(strings.Join(result.Warnings, "; "))
	}

	return nil
}

// secretRemoveOptions is one parsed shard secret rm invocation.
type secretRemoveOptions struct {
	name  string
	force bool
}

func (a App) secretRemove(ctx context.Context, args []string) error {
	opts, err := parseSecretRemove(args)
	if err != nil {
		return err
	}

	if err := a.client().RemoveSecret(ctx, opts.name, opts.force); err != nil {
		return err
	}

	return a.print(opts.name)
}

func parseSecretRemove(args []string) (secretRemoveOptions, error) {
	var opts secretRemoveOptions

	flags := flag.NewFlagSet("shard secret rm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.force, "force", false, "remove the secret even when a sandbox holds it")

	if err := flags.Parse(args); err != nil {
		return secretRemoveOptions{}, fmt.Errorf("parse the secret rm flags: %w", err)
	}

	rest := flags.Args()
	if slices.ContainsFunc(rest, func(s string) bool { return strings.HasPrefix(s, "-") }) {
		return secretRemoveOptions{}, errors.New("secret rm takes its flags before the name: shard secret rm --force <NAME>")
	}
	if len(rest) != 1 {
		return secretRemoveOptions{}, fmt.Errorf("secret rm takes one name, got %d", len(rest))
	}

	opts.name = rest[0]

	return opts, nil
}
