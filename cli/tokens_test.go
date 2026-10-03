package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/serve"
)

func TestTokensMintPrintsARecordTheFrontAccepts(t *testing.T) {
	var out bytes.Buffer

	app, flags, secret := newFrontApp(t, &out)

	// Mint over the front's own secret, so the record lands in the ledger the front reads and the front accepts it.
	if err := app.tokens([]string{"mint", "--name", "ci", "--secret-file", secret}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	printed := strings.TrimSpace(out.String())
	if strings.Contains(printed, "\n") {
		t.Errorf("mint printed %q, want one line", printed)
	}

	var record serve.Token
	if err := json.Unmarshal([]byte(printed), &record); err != nil {
		t.Fatalf("mint printed %q, not one JSON object: %v", printed, err)
	}
	if record.Token == "" {
		t.Error("the record carries no token")
	}
	// No --duration means the token never expires, so the record carries no expiry.
	if record.ExpiresAt != nil {
		t.Errorf("the record expires_at is %v, want none by default", record.ExpiresAt)
	}
	if strings.Join(record.Scopes, ",") != "*" {
		t.Errorf("the record carries scopes %v, want [\"*\"] by default", record.Scopes)
	}

	// The client's --token-file takes the whole record; the bare-token form is covered by the other front tests.
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(printed+"\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	flags[3] = tokenPath

	out.Reset()
	if err := app.Run(t.Context(), append(flags, "ls")); err != nil {
		t.Fatalf("ls with the minted record: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("ls with the minted record printed %q, want the sandbox the daemon holds", out.String())
	}
}

// tokens ls lists a minted token as active and never-expiring, and tokens revoke by id flips it to revoked.
func TestTokensListAndRevokeByID(t *testing.T) {
	var out bytes.Buffer

	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: &out}

	if err := app.tokens([]string{"mint", "--name", "ci", "--secret-file", secret}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	out.Reset()
	if err := app.tokens([]string{"ls", "--secret-file", secret}); err != nil {
		t.Fatalf("ls: %v", err)
	}
	listing := out.String()
	if !strings.Contains(listing, "active") || !strings.Contains(listing, "never") {
		t.Errorf("ls listed %q, want the ci token as active and never-expiring", listing)
	}

	// Pull the id from the listing, then revoke that one id; the flags come before the id.
	id := tokenID(t, listing, "ci")
	out.Reset()
	if err := app.tokens([]string{"revoke", "--secret-file", secret, id}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	out.Reset()
	if err := app.tokens([]string{"ls", "--secret-file", secret}); err != nil {
		t.Fatalf("ls after revoke: %v", err)
	}
	if !strings.Contains(out.String(), "revoked") {
		t.Errorf("ls after revoke listed %q, want the ci token as revoked", out.String())
	}
}

// tokens revoke refuses with no ledger flag, and reports an id the ledger does not hold rather than a silent success.
func TestTokensRevokeRefusesNoLedgerAndAnUnknownID(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: io.Discard}

	if err := app.tokens([]string{"revoke", "some-id"}); err == nil {
		t.Error("revoke ran with no --secret-file or --tokens-file")
	}
	if err := app.tokens([]string{"revoke", "--secret-file", secret, "no-such-id"}); err == nil {
		t.Error("revoke reported success for an id the ledger does not hold")
	}
}

// tokenID pulls the id column out of a tokens ls listing for the row whose name matches.
func tokenID(t *testing.T, listing, name string) string {
	t.Helper()

	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			return fields[0]
		}
	}
	t.Fatalf("no token named %q in the listing %q", name, listing)

	return ""
}

func TestTokensMintRefusesNoName(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	app := App{Version: "test", Root: dir, Out: io.Discard}
	if err := app.tokens([]string{"mint", "--secret-file", secret}); err == nil {
		t.Error("mint signed a token with no name")
	}
}

func TestTokensMintRefusesAShortSecret(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 31)), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	app := App{Version: "test", Root: dir, Out: io.Discard}
	if err := app.tokens([]string{"mint", "--name", "ci", "--secret-file", secret}); err == nil {
		t.Error("mint signed a token with a secret under 32 bytes")
	}
}

// A mistyped scope minted a token that every route refused with 403 and no hint, so mint refuses it, names the scopes it knows, and records nothing (SHARD-373).
func TestTokensMintRefusesAScopeTheFrontDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	app := App{Version: "test", Root: dir, Out: io.Discard}
	err := app.tokens([]string{"mint", "--name", "ci", "--scopes", "sandbox:read,sandbox:raed", "--secret-file", secret})
	if err == nil {
		t.Fatal("mint signed a token with the scope sandbox:raed")
	}
	for _, want := range append([]string{"sandbox:raed"}, everyScope...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q, and it must name %s", err, want)
		}
	}
	if _, err := os.Stat(serve.TokensPath(secret, "")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("mint left a ledger for a token it refused: %v", err)
	}
}

func TestTokensMintTakesEveryScopeTheFrontKnows(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	for _, scope := range everyScope {
		var out bytes.Buffer
		app := App{Version: "test", Root: dir, Out: &out}
		if err := app.tokens([]string{"mint", "--name", "ci", "--scopes", scope, "--secret-file", secret}); err != nil {
			t.Fatalf("mint with the scope %s: %v", scope, err)
		}

		var record serve.Token
		if err := json.Unmarshal(out.Bytes(), &record); err != nil {
			t.Fatalf("mint printed %q, not one JSON object: %v", out.String(), err)
		}
		if strings.Join(record.Scopes, ",") != scope {
			t.Errorf("the record carries scopes %v, want [%s]", record.Scopes, scope)
		}
	}
}

// everyScope is "*" and the eight capabilities docs/daemon.md names.
var everyScope = []string{"*", "daemon:read", "sandbox:read", "sandbox:write", "sandbox:delete", "exec", "image:*", "secret:*", "policy:*"}
