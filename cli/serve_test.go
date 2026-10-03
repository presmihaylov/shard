package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/serve"
)

const frontSecret = "cli-front-secret-0000000000000000"

// newFrontApp puts a fake daemon and a front over it up, records a token in the front's ledger, and answers
// the flags that reach the front and the secret file the front signs and checks with.
func newFrontApp(t *testing.T, out *bytes.Buffer) (App, []string, string) {
	t.Helper()

	return newLoggedFrontApp(t, out, io.Discard)
}

// newLoggedFrontApp is newFrontApp with the front's log lines in frontLog.
func newLoggedFrontApp(t *testing.T, out *bytes.Buffer, frontLog io.Writer) (App, []string, string) {
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

	front, err := serve.New(serve.Config{Listen: "127.0.0.1:0", CertFile: cert, KeyFile: key, SecretFile: secret, Root: app.Root, Out: frontLog})
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
	noRemoteEnv(t)
	t.Setenv(client.RemoteEnv, flags[1])
	t.Setenv(client.TokenFileEnv, flags[3])
	t.Setenv(client.CAFileEnv, flags[5])

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

// An empty SHARD_API_KEY and SHARD_TOKEN_FILE are unset, so the refusal names the three ways in the order they win. (SHARD-464)
func TestAHostWithNoTokenIsRefused(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)
	noRemoteEnv(t)

	err := app.Run(t.Context(), []string{flags[0], flags[1], "ls"})
	if err == nil {
		t.Fatal("a host with no token answered")
	}
	msg := err.Error()
	flag, key, file := strings.Index(msg, "--token-file"), strings.Index(msg, client.APIKeyEnv), strings.Index(msg, client.TokenFileEnv)
	if flag < 0 || key < flag || file < key {
		t.Errorf("a host with no token returned %q, want --token-file, %s and %s in that order", msg, client.APIKeyEnv, client.TokenFileEnv)
	}
}

// A script exports the front and the raw key and nothing else, and the front logs the subject, never the key. (SHARD-464)
func TestTheAPIKeyAloneReachesTheFront(t *testing.T) {
	var out bytes.Buffer
	var frontLog syncBuffer

	app, flags, _ := newLoggedFrontApp(t, &out, &frontLog)
	key := tokenIn(t, flags[3])
	noRemoteEnv(t)
	t.Setenv(client.RemoteEnv, flags[1])
	t.Setenv(client.APIKeyEnv, key)
	t.Setenv(client.CAFileEnv, flags[5])

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls with SHARD_API_KEY alone: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("ls with SHARD_API_KEY printed %q, want the sandbox the daemon holds", out.String())
	}
	if logged := frontLog.String(); !strings.Contains(logged, "authorized") || strings.Contains(logged, key) {
		t.Errorf("the front logged %q, want the authorization and never the key", logged)
	}
}

// A wrong, revoked or expired key reaches the front and gets its 401, as the same token in a file does. (SHARD-464)
func TestAKeyTheFrontDoesNotHonourIsRefusedByTheFront(t *testing.T) {
	var out bytes.Buffer
	var frontLog syncBuffer

	app, flags, secret := newLoggedFrontApp(t, &out, &frontLog)
	ledger := serve.TokensPath(secret, "")
	revoked, err := serve.IssueToken([]byte(frontSecret), ledger, "revoked", nil, time.Hour)
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}
	if _, err := serve.RevokeSubject(ledger, "revoked"); err != nil {
		t.Fatalf("revoke the token: %v", err)
	}
	now := time.Now()
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		ID: "expired-id", Subject: "expired", IssuedAt: jwt.NewNumericDate(now.Add(-2 * time.Hour)), ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
	}).SignedString([]byte(frontSecret))
	if err != nil {
		t.Fatalf("sign an expired token: %v", err)
	}

	for name, key := range map[string]string{
		"a wrong key":       "shard464-synthetic-wrong-key",
		"a revoked key":     revoked.Token,
		"an expired key":    expired,
		"a key of no token": "shard464 synthetic {key}",
	} {
		t.Run(name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, flags[1])
			t.Setenv(client.APIKeyEnv, key)
			t.Setenv(client.CAFileEnv, flags[5])

			err := app.Run(t.Context(), []string{"ls"})
			if err == nil || !strings.Contains(err.Error(), "no valid bearer token") || strings.Contains(err.Error(), key) {
				t.Errorf("ls with %s returned %v, want the refusal of the front and never the key", name, err)
			}
			if strings.Contains(frontLog.String(), key) {
				t.Errorf("the front logged the key: %q", frontLog.String())
			}
		})
	}
}

// The token comes from --token-file, then SHARD_API_KEY, then SHARD_TOKEN_FILE; a source that wins with the wrong token is refused. (SHARD-464)
func TestTheTokenOrderReachesTheFront(t *testing.T) {
	var out bytes.Buffer

	app, flags, _ := newFrontApp(t, &out)
	good := flags[3]
	key := tokenIn(t, good)
	bad := filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(bad, []byte("not-the-token\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}

	for _, tc := range []struct {
		name               string
		flag, key, envFile string
		ok                 bool
	}{
		{name: "the flag beats the key", flag: good, key: "not-the-key", ok: true},
		{name: "the flag beats a key that would pass", flag: bad, key: key},
		{name: "the key beats the env file", key: key, envFile: bad, ok: true},
		{name: "the key beats an env file that would pass", key: "not-the-key", envFile: good},
		{name: "the flag beats the env file", flag: good, envFile: bad, ok: true},
		{name: "the flag beats an env file that would pass", flag: bad, envFile: good},
		{name: "the flag beats both", flag: good, key: "not-the-key", envFile: bad, ok: true},
		{name: "the flag beats both that would pass", flag: bad, key: key, envFile: good},
		{name: "an empty key is unset", key: "", envFile: good, ok: true},
		{name: "a blank key is unset", key: " \t\n", envFile: good, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.APIKeyEnv, tc.key)
			t.Setenv(client.TokenFileEnv, tc.envFile)

			args := []string{flags[0], flags[1], flags[4], flags[5]}
			if tc.flag != "" {
				args = append(args, "--token-file", tc.flag)
			}

			err := app.Run(t.Context(), append(args, "ls"))
			if tc.ok && err != nil {
				t.Errorf("ls returned %v, want the answer of the daemon", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "no valid bearer token")) {
				t.Errorf("ls returned %v, want the refusal of the front", err)
			}
		})
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

// noRemoteEnv unsets every variable a remote verb reads, so the shell that runs the test decides nothing.
func noRemoteEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{client.RemoteEnv, client.APIKeyEnv, client.TokenFileEnv, client.CAFileEnv} {
		t.Setenv(name, "")
	}
}

// tokenIn is the bare token a token file holds, which is what SHARD_API_KEY takes.
func tokenIn(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the token file: %v", err)
	}

	return strings.TrimSpace(string(raw))
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
