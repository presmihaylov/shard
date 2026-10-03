package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/presmihaylov/shard/services/serve"
)

// tokensMint signs one token for a subject, records it in the ledger, and prints the record. The daemon never sees the secret.
func (a App) tokensMint(_ context.Context, args []string) error {
	flags := newFlags("tokens mint")
	name := flags.String("name", "", "")
	duration := flags.Duration("duration", 0, "")
	secretFile := flags.String("secret-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")
	scopes := flags.String("scopes", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
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
	if *secretFile == "" {
		return errors.New("tokens mint needs --secret-file: it holds the secret that signs the token")
	}

	secret, err := serve.ReadSecret(*secretFile)
	if err != nil {
		return fmt.Errorf("tokens mint: %w", err)
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
func (a App) tokensList(_ context.Context, args []string) error {
	flags := newFlags("tokens ls")
	secretFile := flags.String("secret-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
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
func (a App) tokensRevoke(_ context.Context, args []string) error {
	flags := newFlags("tokens revoke")
	secretFile := flags.String("secret-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")
	name := flags.String("name", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
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
