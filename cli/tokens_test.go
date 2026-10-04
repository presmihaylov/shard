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

	app, f, secret := newFrontApp(t, &out)

	// Mint over the front's own signing key, so the record lands in the ledger the front reads and the front accepts it.
	if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--signing-key-file", secret}); err != nil {
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

	// SHARD_API_KEY takes the token field of the record.
	f.key = record.Token

	out.Reset()
	if err := app.Run(t.Context(), append(f.use(t), "list")); err != nil {
		t.Fatalf("list with the minted record: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list with the minted record printed %q, want the sandbox the daemon holds", out.String())
	}
}

// tokens list lists a minted token as active and never-expiring, and tokens revoke by id flips it to revoked.
func TestTokensListAndRevokeByID(t *testing.T) {
	var out bytes.Buffer

	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: &out}

	if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--signing-key-file", secret}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"tokens", "list", "--signing-key-file", secret}); err != nil {
		t.Fatalf("list: %v", err)
	}
	listing := out.String()
	if !strings.Contains(listing, "active") || !strings.Contains(listing, "never") {
		t.Errorf("list listed %q, want the ci token as active and never-expiring", listing)
	}

	// Pull the id from the listing, then revoke that one id; the flags come before the id.
	id := tokenID(t, listing, "ci")
	out.Reset()
	if err := app.Run(t.Context(), []string{"tokens", "revoke", "--signing-key-file", secret, id}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"tokens", "list", "--signing-key-file", secret}); err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if !strings.Contains(out.String(), "revoked") {
		t.Errorf("list after revoke listed %q, want the ci token as revoked", out.String())
	}
}

// tokens revoke reports an id the ledger does not hold rather than a silent success.
func TestTokensRevokeRefusesAnUnknownID(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: io.Discard}

	if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--signing-key-file", secret}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	err := app.Run(t.Context(), []string{"tokens", "revoke", "--signing-key-file", secret, "no-such-id"})
	if err == nil || !strings.Contains(err.Error(), "no token with id no-such-id") {
		t.Errorf("revoke of an id the ledger does not hold returned %v, want a refusal that names it", err)
	}
}

// mint with no key flag creates the default key, and a front started with no key flag checks tokens over the same one.
func TestTokensMintAndServeShareTheDefaultSigningKey(t *testing.T) {
	var out bytes.Buffer
	app := newListApp(t, &out, listed(), nil)

	if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "build-agent", "--duration", "24h"}); err != nil {
		t.Fatalf("mint with no key flag: %v", err)
	}
	if info, err := os.Stat(filepath.Join(app.Root, "auth", "signing-key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mint left no 0600 key at the default path: %v", err)
	}
	var record serve.Token
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatalf("mint printed %q, not one JSON object: %v", out.String(), err)
	}

	address, cert := startFront(t, serve.Config{Listen: "127.0.0.1:0", Root: app.Root, Out: io.Discard}, true)
	f := front{url: "https://" + address, key: record.Token, ca: cert}

	out.Reset()
	if err := app.Run(t.Context(), append(f.use(t), "list")); err != nil {
		t.Fatalf("list through the front with the minted token: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list through the front printed %q, want the sandbox the daemon holds", out.String())
	}

	out.Reset()
	if err := app.Run(t.Context(), []string{"tokens", "list"}); err != nil {
		t.Fatalf("tokens list with no flag: %v", err)
	}
	if !strings.Contains(out.String(), "build-agent") {
		t.Errorf("tokens list with no flag listed %q, want the token mint recorded", out.String())
	}
}

// list and revoke need only the ledger, so on a root with no key they report an empty ledger and create nothing.
func TestTokensListAndRevokeCreateNoSigningKey(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	app := App{Version: "test", Root: dir, Out: &out}

	if err := app.Run(t.Context(), []string{"tokens", "list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "ID") {
		t.Errorf("list printed %q, want the header alone", out.String())
	}
	err := app.Run(t.Context(), []string{"tokens", "revoke", "some-id"})
	if err == nil || !strings.Contains(err.Error(), "no token with id some-id") {
		t.Errorf("revoke returned %v, want a refusal that names the id", err)
	}
	if err := app.Run(t.Context(), []string{"tokens", "revoke", "--name", "ci"}); err != nil {
		t.Errorf("revoke --name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "auth")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("list or revoke created %s/auth: %v", dir, err)
	}
}

// A named key file is never generated: every verb refuses one that does not exist, names the flag and the path, and creates nothing.
func TestTokensRefuseAMissingSigningKeyFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "signing-key")
	app := App{Version: "test", Root: dir, Out: io.Discard}

	for _, args := range [][]string{
		{"tokens", "mint", "--name", "ci", "--signing-key-file", missing},
		{"tokens", "list", "--signing-key-file", missing},
		{"tokens", "revoke", "--signing-key-file", missing, "some-id"},
	} {
		err := app.Run(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "--signing-key-file") || !strings.Contains(err.Error(), missing) {
			t.Errorf("%s returned %v, want a refusal that names --signing-key-file and %s", strings.Join(args[:2], " "), err, missing)
		}
	}
	for _, path := range []string{missing, filepath.Join(dir, "auth")} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a refused verb created %s: %v", path, err)
		}
	}
}

// tokenID pulls the id column out of a tokens list listing for the row whose name matches.
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
	if err := app.Run(t.Context(), []string{"tokens", "mint", "--signing-key-file", secret}); err == nil {
		t.Error("mint signed a token with no name")
	}
}

func TestTokensMintRefusesAShortSigningKey(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 31)), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	app := App{Version: "test", Root: dir, Out: io.Discard}
	err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--signing-key-file", secret})
	if err == nil {
		t.Fatal("mint signed a token with a signing key under 32 bytes")
	}
	// The refusal names the verb that ran and the bound, never shard serve.
	if msg := err.Error(); !strings.HasPrefix(msg, "tokens mint: ") || !strings.Contains(msg, "31 bytes") || strings.Contains(msg, "shard serve") {
		t.Errorf("the refusal is %q, want tokens mint, the byte count and no shard serve", msg)
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
	err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--scopes", "sandbox:read,sandbox:raed", "--signing-key-file", secret})
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

// A mint refused for its scope never needed the key, so it creates no default one.
func TestTokensMintRefusedForAScopeCreatesNoSigningKey(t *testing.T) {
	dir := t.TempDir()
	app := App{Version: "test", Root: dir, Out: io.Discard}

	if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--scopes", "sandbox:raed"}); err == nil {
		t.Fatal("mint signed a token with the scope sandbox:raed")
	}
	if _, err := os.Stat(filepath.Join(dir, "auth")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused mint created %s/auth: %v", dir, err)
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
		if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--scopes", scope, "--signing-key-file", secret}); err != nil {
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

// No public route needs daemon:read or image:*, so mint refuses both.
func TestTokensMintRefusesTheRetiredScopes(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	for _, scope := range []string{"daemon:read", "image:*"} {
		app := App{Version: "test", Root: dir, Out: io.Discard}
		if err := app.Run(t.Context(), []string{"tokens", "mint", "--name", "ci", "--scopes", scope, "--signing-key-file", secret}); err == nil {
			t.Errorf("mint signed a token with the retired scope %s", scope)
		}
	}
}

// everyScope is "*" and the six scopes docs/daemon.md names.
var everyScope = []string{"*", "sandbox:read", "sandbox:write", "sandbox:delete", "exec", "secret:*", "policy:*"}
