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
	// placeholder overrides the default; empty leaves the default, or the stored one on a rotation.
	placeholder string
	// value is what followed the name, and hasValue tells an empty one from none at all.
	value    string
	hasValue bool
}

// cautionOnArgv is printed once, exactly, when the value came from argv: ps showed it while the command ran.
const cautionOnArgv = "caution: the value was on the command line and visible in the process list while the command ran; pipe it on stdin to avoid that"

func (a App) secretSet(ctx context.Context, args []string) error {
	opts, err := parseSecretSet(args)
	if err != nil {
		return err
	}

	value, err := a.secretValue(opts)
	if err != nil {
		return err
	}

	sec, err := a.client().SetSecret(ctx, opts.name, value, opts.destinations, opts.placeholder)
	if err != nil {
		return err
	}

	return a.print(sec.Name)
}

// secretValue takes the value three ways: after the name, on stdin, or from a prompt with the echo off.
func (a App) secretValue(opts secretSetOptions) (string, error) {
	if opts.hasValue && opts.value != "-" {
		if a.Err != nil {
			fmt.Fprintln(a.Err, cautionOnArgv)
		}

		return opts.value, nil
	}

	if opts.value == "-" || !pty.IsTerminal(a.stdin()) {
		return readSecretValue(a.stdin())
	}

	return a.promptSecretValue(opts.name)
}

// promptSecretValue asks the terminal with the echo off, so the value lands in no history and on no screen.
func (a App) promptSecretValue(name string) (string, error) {
	if a.Err != nil {
		fmt.Fprintf(a.Err, "value for %s: ", name)
	}

	blob, err := pty.ReadPassword(a.stdin())
	if a.Err != nil {
		fmt.Fprintln(a.Err)
	}
	if err != nil {
		return "", fmt.Errorf("read the secret value from the terminal: %w", err)
	}

	value := string(blob)
	if value == "" {
		return "", fmt.Errorf("the secret value of %s is empty", name)
	}

	return value, nil
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
	flags.StringVar(&opts.placeholder, "placeholder", "", "what the guest holds in place of the value, default mock-NAME")

	if err := flags.Parse(args); err != nil {
		return secretSetOptions{}, fmt.Errorf("parse the secret set flags: %w", err)
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return secretSetOptions{}, errors.New("secret set takes a name and an optional value, got none")
	}
	// A value that starts with - needs a -- before it, so anything else that does is a misplaced flag.
	if strings.HasPrefix(rest[0], "-") || (len(rest) > 2 && strings.HasPrefix(rest[1], "-")) {
		return secretSetOptions{}, errors.New("secret set takes its flags before the name: shard secret set --to <host> [--placeholder <string>] <NAME> [VALUE], with -- before a value that starts with -")
	}
	if len(rest) > 2 {
		return secretSetOptions{}, fmt.Errorf("secret set takes a name and an optional value, got %d arguments", len(rest))
	}

	opts.name = rest[0]
	if len(rest) == 2 {
		opts.value, opts.hasValue = rest[1], true
	}

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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", sec.Name, strings.Join(sec.Destinations, ","), sec.Placeholder, humanAge(sec.UpdatedAt))
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
