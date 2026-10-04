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

// tokensMint signs one token for a subject, records it in the ledger, and prints the record. The daemon never sees the signing key.
func (a App) tokensMint(_ context.Context, args []string) error {
	flags := newFlags("tokens mint")
	name := flags.String("name", "", "")
	duration := flags.Duration("duration", 0, "")
	signingKeyFile := flags.String("signing-key-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")
	scopes := flags.String("scopes", "", "")
	format := addFormatFlag(flags, formatJSON)

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
	// A refused mint needs no key, so it never creates the default one.
	scopeList := parseScopes(*scopes)
	if err := serve.CheckScopes(scopeList); err != nil {
		return fmt.Errorf("tokens mint: %w", err)
	}

	signingKey, keyPath, err := serve.SigningKey(a.Root, *signingKeyFile)
	if err != nil {
		return fmt.Errorf("tokens mint: %w", err)
	}

	minted, err := serve.IssueToken(signingKey, serve.TokensPath(keyPath, *tokensFile), *name, scopeList, *duration)
	if err != nil {
		return err
	}
	if *format == formatTable {
		return writeSections(a.Out, mintSection(minted))
	}

	record, err := json.Marshal(minted)
	if err != nil {
		return fmt.Errorf("encode the mint record: %w", err)
	}

	return a.print(string(record))
}

// tokensList lists every token the ledger records, with the status a request would see now.
func (a App) tokensList(_ context.Context, args []string) error {
	flags := newFlags("tokens list")
	signingKeyFile := flags.String("signing-key-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")
	format := addFormatFlag(flags, formatTable)

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tokens list takes no arguments, got %d", flags.NArg())
	}
	path, err := a.ledgerPath(*signingKeyFile, *tokensFile)
	if err != nil {
		return fmt.Errorf("tokens list: %w", err)
	}

	infos, err := serve.ListTokens(path)
	if err != nil {
		return err
	}
	if *format == formatJSON {
		return writeJSON(a.Out, tokenViews(infos))
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
	signingKeyFile := flags.String("signing-key-file", "", "")
	tokensFile := flags.String("tokens-file", "", "")
	name := flags.String("name", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
	}

	path, err := a.ledgerPath(*signingKeyFile, *tokensFile)
	if err != nil {
		return fmt.Errorf("tokens revoke: %w", err)
	}
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

// ledgerPath is the ledger list and revoke use: --tokens-file, else the one beside the signing key file. It creates nothing.
func (a App) ledgerPath(signingKeyFile, tokensFile string) (string, error) {
	keyPath, err := serve.SigningKeyPath(a.Root, signingKeyFile)
	if err != nil {
		return "", err
	}

	return serve.TokensPath(keyPath, tokensFile), nil
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
