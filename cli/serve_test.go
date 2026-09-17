package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/serve"
)

const frontSecret = "cli-front-secret-0000000000000000"

// newFrontApp puts a fake daemon and a front over it up, records a token in the front's ledger, and answers
// the flags that reach the front and the secret file the front signs and checks with.
func newFrontApp(t *testing.T, out *bytes.Buffer) (App, []string, string) {
	t.Helper()

	app := newLsApp(t, out, listed(), nil)

	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	minted, err := serve.IssueToken([]byte(frontSecret), serve.TokensPath(secret, ""), "cli", nil, time.Hour)
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte(minted.Token+"\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}

	cert, key := selfSigned(t, dir)

	front, err := serve.New(serve.Config{Listen: "127.0.0.1:0", CertFile: cert, KeyFile: key, SecretFile: secret, Root: app.Root, Out: io.Discard})
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	listener, err := front.Listen()
	if err != nil {
		t.Fatalf("serve.Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() { ended <- front.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-ended; err != nil {
			t.Errorf("the front ended with %v", err)
		}
	})

	return app, []string{"--remote", "https://" + listener.Addr().String(), "--token-file", token, "--ca-file", cert}, secret
}

// The remote front comes from SHARD_REMOTE too, so a shell exports it once. (SHARD-194)
func TestTheRemoteEnvReachesTheFront(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)
	t.Setenv(RemoteEnv, flags[1])
	t.Setenv(TokenFileEnv, flags[3])
	t.Setenv(CAFileEnv, flags[5])

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls through the front from the env: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("ls from the env printed %q, want the sandbox the daemon holds", out.String())
	}
}

func TestAVerbReachesTheDaemonThroughTheFront(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)

	if err := app.Run(t.Context(), append(flags, "ls")); err != nil {
		t.Fatalf("ls through the front: %v", err)
	}

	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("ls through the front printed %q, want the sandbox the daemon holds", out.String())
	}
}

func TestAVerbWithTheWrongTokenIsRefusedByTheFront(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)

	wrong := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(wrong, []byte("not-the-token"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	flags[3] = wrong

	err := app.Run(t.Context(), append(flags, "ls"))
	if err == nil {
		t.Fatal("ls with the wrong token answered")
	}
	if !strings.Contains(err.Error(), "no valid bearer token") {
		t.Errorf("ls with the wrong token returned %v, want the refusal of the front", err)
	}
}

func TestAHostThatIsNotHTTPSIsRefused(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)
	flags[1] = "http://127.0.0.1:2376"

	err := app.Run(t.Context(), append(flags, "ls"))
	if err == nil || !strings.Contains(err.Error(), "https url") {
		t.Errorf("a plain http host returned %v, want a refusal", err)
	}
}

func TestAHostWithNoTokenFileIsRefused(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)

	err := app.Run(t.Context(), []string{flags[0], flags[1], "ls"})
	if err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Errorf("a host with no token file returned %v, want a refusal", err)
	}
}

func TestServeRefusesArgumentsAndAPairItLacks(t *testing.T) {
	app := App{Version: "test", Root: t.TempDir(), Out: io.Discard}

	if err := app.serve(t.Context(), []string{"127.0.0.1:2376"}); err == nil {
		t.Error("serve took an argument")
	}
	if err := app.serve(t.Context(), []string{"--listen", "127.0.0.1:0"}); err == nil {
		t.Error("serve started with no certificate and no key")
	}
}

func TestServeMintPrintsARecordTheFrontAccepts(t *testing.T) {
	var out bytes.Buffer

	app, flags, secret := newFrontApp(t, &out)

	// Mint over the front's own secret, so the record lands in the ledger the front reads and the front accepts it.
	if err := app.serve(t.Context(), []string{"mint", "--name", "ci", "--secret-file", secret}); err != nil {
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

// serve tokens lists a minted token as active and never-expiring, and serve revoke by id flips it to revoked.
func TestServeTokensListsAndRevokesByID(t *testing.T) {
	var out bytes.Buffer

	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: &out}

	if err := app.serve(t.Context(), []string{"mint", "--name", "ci", "--secret-file", secret}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	out.Reset()
	if err := app.serve(t.Context(), []string{"tokens", "--secret-file", secret}); err != nil {
		t.Fatalf("tokens: %v", err)
	}
	listing := out.String()
	if !strings.Contains(listing, "active") || !strings.Contains(listing, "never") {
		t.Errorf("tokens listed %q, want the ci token as active and never-expiring", listing)
	}

	// Pull the id from the listing, then revoke that one id; the flags come before the id.
	id := tokenID(t, listing, "ci")
	out.Reset()
	if err := app.serve(t.Context(), []string{"revoke", "--secret-file", secret, id}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	out.Reset()
	if err := app.serve(t.Context(), []string{"tokens", "--secret-file", secret}); err != nil {
		t.Fatalf("tokens after revoke: %v", err)
	}
	if !strings.Contains(out.String(), "revoked") {
		t.Errorf("tokens after revoke listed %q, want the ci token as revoked", out.String())
	}
}

// serve revoke refuses with no ledger flag, and reports an id the ledger does not hold rather than a silent success.
func TestServeRevokeRefusesNoLedgerAndAnUnknownID(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}
	app := App{Version: "test", Root: dir, Out: io.Discard}

	if err := app.serve(t.Context(), []string{"revoke", "some-id"}); err == nil {
		t.Error("revoke ran with no --secret-file or --tokens-file")
	}
	if err := app.serve(t.Context(), []string{"revoke", "--secret-file", secret, "no-such-id"}); err == nil {
		t.Error("revoke reported success for an id the ledger does not hold")
	}
}

// tokenID pulls the id column out of a serve tokens listing for the row whose name matches.
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

func TestServeMintRefusesNoName(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(frontSecret), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	app := App{Version: "test", Root: dir, Out: io.Discard}
	if err := app.serve(t.Context(), []string{"mint", "--secret-file", secret}); err == nil {
		t.Error("mint signed a token with no name")
	}
}

func TestServeMintRefusesAShortSecret(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 31)), 0o600); err != nil {
		t.Fatalf("write the secret file: %v", err)
	}

	app := App{Version: "test", Root: dir, Out: io.Discard}
	if err := app.serve(t.Context(), []string{"mint", "--name", "ci", "--secret-file", secret}); err == nil {
		t.Error("mint signed a token with a secret under 32 bytes")
	}
}

// selfSigned writes a certificate for 127.0.0.1 and its key into dir, and answers the two paths.
func selfSigned(t *testing.T, dir string) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign the certificate: %v", err)
	}

	marshalled, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}

	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: marshalled}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	return certPath, keyPath
}
