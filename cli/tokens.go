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
	scopes := flags.String("scopes", "", "")
	format := addFormatFlag(flags, formatJSON)

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tokens mint takes no arguments, got %s", gotArgs(flags.Args()))
	}
	if err := a.hostOnly("tokens mint"); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("tokens mint needs --name: who or what uses the token, as --name ci")
	}
	if *duration < 0 {
		return fmt.Errorf("--duration must not be negative, got %s; leave it out for a token that never expires", short(*duration))
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

	minted, err := serve.IssueToken(signingKey, serve.TokensPath(keyPath), *name, scopeList, *duration)
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
	format := addFormatFlag(flags, formatTable)

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tokens list takes no arguments, got %s", gotArgs(flags.Args()))
	}
	if err := a.hostOnly("tokens list"); err != nil {
		return err
	}
	path, err := a.ledgerPath(*signingKeyFile)
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
func (a App) tokensRevoke(_ context.Context, args []string) error {
	flags := newFlags("tokens revoke")
	signingKeyFile := flags.String("signing-key-file", "", "")
	name := flags.String("name", "", "")

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if err := a.hostOnly("tokens revoke"); err != nil {
		return err
	}

	path, err := a.ledgerPath(*signingKeyFile)
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

		return a.print(fmt.Sprintf("revoked %d token(s) named %s", found, *name))
	}

	if flags.NArg() != 1 {
		return fmt.Errorf("tokens revoke takes one token id, got %s", gotArgs(flags.Args()))
	}
	id := flags.Arg(0)
	found, err := serve.RevokeToken(path, id)
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("no token with id %s; shard tokens list shows the ids", id)
	}

	return a.print(fmt.Sprintf("revoked token %s", id))
}

// tokensScopes lists the scopes the server it speaks to accepts, so unlike mint, list and revoke it follows --remote.
func (a App) tokensScopes(ctx context.Context, args []string) error {
	flags := newFlags("tokens scopes")
	format := addFormatFlag(flags, formatTable)

	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tokens scopes takes no arguments, got %s", gotArgs(flags.Args()))
	}
	c, err := a.client()
	if err != nil {
		return err
	}

	scopes, err := c.Scopes(ctx)
	if err != nil {
		return err
	}
	if *format == formatJSON {
		return writeJSON(a.Out, scopes)
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "SCOPE\tDESCRIPTION")
	for _, s := range scopes.Scopes {
		fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Description)
	}

	return w.Flush()
}

// ledgerPath is the ledger beside the signing key file, for list and revoke. It creates nothing.
func (a App) ledgerPath(signingKeyFile string) (string, error) {
	keyPath, err := serve.SigningKeyPath(a.Root, signingKeyFile)
	if err != nil {
		return "", err
	}

	return serve.TokensPath(keyPath), nil
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
