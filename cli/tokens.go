package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/services/serve"
)

// tokens groups the local verbs that mint, list and revoke the tokens the front checks. None reaches the daemon.
func (a App) tokens(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("tokens takes a subcommand: mint, ls or revoke")
	}

	switch args[0] {
	case "mint":
		return a.tokensMint(args[1:])
	case "ls", "list":
		return a.tokensList(args[1:])
	case "revoke":
		return a.tokensRevoke(args[1:])
	}

	return fmt.Errorf("unknown tokens subcommand %q; want mint, ls or revoke", args[0])
}

// tokensMint signs one token for a subject, records it in the ledger, and prints the record. The daemon never sees the secret.
func (a App) tokensMint(args []string) error {
	flags := flag.NewFlagSet("tokens mint", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "the subject the token names")
	duration := flags.Duration("duration", 0, "how long the token is valid; the default, 0, never expires")
	secretFile := flags.String("secret-file", "", "the file holding the secret that signs the token")
	tokensFile := flags.String("tokens-file", "", "the ledger to record the token in; overrides the one beside the secret file")
	scopes := flags.String("scopes", "", "a comma-separated list of scopes the token carries; empty is every verb")

	if err := parseVerb(flags, args); err != nil {
		return fmt.Errorf("parse the tokens mint flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tokens mint takes no arguments, got %d", flags.NArg())
	}
	if *name == "" {
		return errors.New("tokens mint needs --name: it is the subject of the token")
	}
	if *duration < 0 {
		return fmt.Errorf("tokens mint needs a --duration in the future, got %s", *duration)
	}

	secret, err := serve.ReadSecret(*secretFile)
	if err != nil {
		return err
	}

	minted, err := serve.IssueToken(secret, serve.TokensPath(*secretFile, *tokensFile), *name, parseScopes(*scopes), *duration)
	if err != nil {
		return err
	}

	record, err := json.Marshal(minted)
	if err != nil {
		return fmt.Errorf("encode the mint record: %w", err)
	}

	return a.print(string(record))
}

// tokensList lists every token the ledger records, with the status a request would see now.
func (a App) tokensList(args []string) error {
	flags := flag.NewFlagSet("tokens ls", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	secretFile := flags.String("secret-file", "", "the file whose directory holds the ledger")
	tokensFile := flags.String("tokens-file", "", "the ledger file; overrides the one beside the secret file")

	if err := parseVerb(flags, args); err != nil {
		return fmt.Errorf("parse the tokens ls flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tokens ls takes no arguments, got %d", flags.NArg())
	}
	if *secretFile == "" && *tokensFile == "" {
		return errors.New("tokens ls needs --secret-file or --tokens-file: it names the ledger to read")
	}

	infos, err := serve.ListTokens(serve.TokensPath(*secretFile, *tokensFile))
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tISSUED\tEXPIRES\tSCOPES\tSTATUS")
	for _, info := range infos {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			info.ID, info.Subject, humanAge(info.IssuedAt), expiresText(info.ExpiresAt),
			strings.Join(info.Scopes, ","), info.Status)
	}

	return w.Flush()
}

// tokensRevoke marks a token revoked by its id, or every token of a subject with --name, so the next request fails.
// Put the flags before the id: flag parsing stops at the first argument.
func (a App) tokensRevoke(args []string) error {
	flags := flag.NewFlagSet("tokens revoke", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	secretFile := flags.String("secret-file", "", "the file whose directory holds the ledger")
	tokensFile := flags.String("tokens-file", "", "the ledger file; overrides the one beside the secret file")
	name := flags.String("name", "", "revoke every token of this subject instead of one id")

	if err := parseVerb(flags, args); err != nil {
		return fmt.Errorf("parse the tokens revoke flags: %w", err)
	}
	if *secretFile == "" && *tokensFile == "" {
		return errors.New("tokens revoke needs --secret-file or --tokens-file: it names the ledger to write")
	}

	path := serve.TokensPath(*secretFile, *tokensFile)
	if *name != "" {
		if flags.NArg() != 0 {
			return errors.New("tokens revoke takes either --name or an id, not both")
		}
		found, err := serve.RevokeSubject(path, *name)
		if err != nil {
			return err
		}

		return a.print(fmt.Sprintf("revoked %d tokens of %s", found, *name))
	}

	if flags.NArg() != 1 {
		return fmt.Errorf("tokens revoke needs one token id, got %d; put the flags before the id", flags.NArg())
	}
	id := flags.Arg(0)
	found, err := serve.RevokeToken(path, id)
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("the ledger holds no token with id %s", id)
	}

	return a.print(fmt.Sprintf("revoked token %s", id))
}

// expiresText is when a token expires, in RFC3339, or "never" for a token with no expiry.
func expiresText(t *time.Time) string {
	if t == nil {
		return "never"
	}

	return t.Format(time.RFC3339)
}

// parseScopes splits a comma-separated --scopes into the list Mint carries; empty is nil, which is every verb.
func parseScopes(raw string) []string {
	var scopes []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			scopes = append(scopes, part)
		}
	}

	return scopes
}
