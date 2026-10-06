package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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

	"github.com/golang-jwt/jwt/v5"

	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/serve"
)

const frontSecret = "cli-front-secret-0000000000000000"

// front is a serve front a test reaches: its url, a key its ledger honours, and the CA that verifies it, empty over http.
type front struct {
	url, key, ca string
}

// use exports the key and the CA as a shell would, and answers the flag that names the front.
func (f front) use(t *testing.T) []string {
	t.Helper()

	noRemoteEnv(t)
	t.Setenv(client.APIKeyEnv, f.key)
	t.Setenv(client.CAFileEnv, f.ca)

	return []string{"--remote", f.url}
}

// newFrontApp answers an https front over a fake daemon, with a key its ledger holds, so no test needs a live daemon.
func newFrontApp(t *testing.T, out *bytes.Buffer) (App, front, string) {
	t.Helper()

	return newLoggedFrontApp(t, out, io.Discard, true)
}

// newLoggedFrontApp is newFrontApp with the front's log lines in frontLog, over https when secure and over http, as serve answers on loopback, when not.
func newLoggedFrontApp(t *testing.T, out *bytes.Buffer, frontLog io.Writer, secure bool) (App, front, string) {
	t.Helper()

	app := newListApp(t, out, listed(), nil)

	secret := filepath.Join(t.TempDir(), "signing-key")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the signing key file: %v", err)
	}

	minted, err := serve.IssueToken([]byte(frontSecret), serve.TokensPath(secret), "cli", nil, time.Hour)
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	address, cert := startFront(t, serve.Config{Listen: "127.0.0.1:0", SigningKeyFile: secret, Root: app.Root, Out: frontLog}, secure)
	if !secure {
		return app, front{url: "http://" + address, key: minted.Token}, secret
	}

	return app, front{url: "https://" + address, key: minted.Token, ca: cert}, secret
}

// startFront serves one front over cfg until the test ends, behind TLS in place of the proxy when secure, and answers the address it bound and the certificate, empty over http.
func startFront(t *testing.T, cfg serve.Config, secure bool) (string, string) {
	t.Helper()

	server, err := serve.New(cfg)
	if err != nil {
		t.Fatalf("serve.New: %v", err)
	}

	listener, err := server.Listen()
	if err != nil {
		t.Fatalf("serve.Listen: %v", err)
	}

	served, cert := listener, ""
	if secure {
		certPath, key := selfSigned(t, t.TempDir())
		pair, err := tls.LoadX509KeyPair(certPath, key)
		if err != nil {
			t.Fatalf("load the key pair: %v", err)
		}
		served, cert = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}), certPath
	}

	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() { ended <- server.Serve(ctx, served) }()
	t.Cleanup(func() {
		cancel()
		if err := <-ended; err != nil {
			t.Errorf("the front ended with %v", err)
		}
	})

	return listener.Addr().String(), cert
}

// The remote front comes from SHARD_REMOTE too, so a shell exports it once. (SHARD-194)
func TestTheRemoteEnvReachesTheFront(t *testing.T) {
	var out bytes.Buffer

	app, f, _ := newFrontApp(t, &out)
	f.use(t)
	t.Setenv(client.RemoteEnv, f.url)

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list through the front from the env: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list from the env printed %q, want the sandbox the daemon holds", out.String())
	}
}

func TestAVerbReachesTheDaemonThroughTheFront(t *testing.T) {
	var out bytes.Buffer

	app, f, _ := newFrontApp(t, &out)

	if err := app.Run(t.Context(), append(f.use(t), "list")); err != nil {
		t.Fatalf("list through the front: %v", err)
	}

	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list through the front printed %q, want the sandbox the daemon holds", out.String())
	}
}

// An http remote works, and its one warning goes to stderr once a run, so the JSON on stdout still parses; https prints none. (SHARD-503)
func TestAnHTTPRemoteWarnsOnStderrOnce(t *testing.T) {
	for _, secure := range []bool{false, true} {
		var out, warnings bytes.Buffer

		app, f, _ := newLoggedFrontApp(t, &out, io.Discard, secure)
		app.Err = &warnings

		if err := app.Run(t.Context(), append(f.use(t), "list", "--format", "json")); err != nil {
			t.Fatalf("list --format json through %s: %v", f.url, err)
		}
		var sandboxes []map[string]any
		if err := json.Unmarshal(out.Bytes(), &sandboxes); err != nil || !strings.Contains(out.String(), "up-1") {
			t.Errorf("list --format json through %s printed %q, want the JSON of the sandboxes alone: %v", f.url, out.String(), err)
		}

		want := ""
		if !secure {
			want = plainWarning + "\n"
		}
		if warnings.String() != want {
			t.Errorf("list through %s warned %q on stderr, want %q", f.url, warnings.String(), want)
		}
	}
}

// The token file and the CA flag are gone, with no alias and no pointer to what replaced them. (SHARD-503)
func TestTheRemovedCredentialFlagsAreUnknown(t *testing.T) {
	var out bytes.Buffer

	app, f, _ := newFrontApp(t, &out)

	for _, removed := range []string{"--token-file", "--ca-file"} {
		err := app.Run(t.Context(), append(f.use(t), removed, f.ca, "list"))
		if want := "unknown flag " + removed + "; run shard --help"; err == nil || err.Error() != want {
			t.Errorf("%s returned %v, want %q", removed, err, want)
		}
	}
}

// An https front behind a private CA is refused on the default trust store, so verification is never dropped. (SHARD-503)
func TestAnHTTPSFrontOfAPrivateCANeedsSHARDCAFILE(t *testing.T) {
	var out bytes.Buffer

	app, f, _ := newFrontApp(t, &out)
	args := f.use(t)
	t.Setenv(client.CAFileEnv, "")

	err := app.Run(t.Context(), append(args, "list"))
	if err == nil || !strings.Contains(err.Error(), "is not trusted") {
		t.Errorf("list with no %s returned %v, want an untrusted certificate", client.CAFileEnv, err)
	}
}

// SHARD_CA_FILE with an http remote is refused before a connection opens, naming both. (SHARD-503)
func TestSHARDCAFILEWithAnHTTPRemoteFailsBeforeItDials(t *testing.T) {
	accepted := acceptCount(t)
	cert, _ := selfSigned(t, t.TempDir())
	noRemoteEnv(t)
	t.Setenv(client.APIKeyEnv, "shard503-synthetic-key")
	t.Setenv(client.CAFileEnv, cert)

	remote := "http://" + accepted.address
	app := App{Version: "test", Root: t.TempDir(), Out: io.Discard, Err: io.Discard}
	err := app.Run(t.Context(), []string{"--remote", remote, "list"})
	if err == nil || !strings.Contains(err.Error(), client.CAFileEnv) || !strings.Contains(err.Error(), remote) {
		t.Errorf("list with %s and %s returned %v, want a refusal that names both", client.CAFileEnv, remote, err)
	}
	accepted.none(t)
}

// SHARD_API_KEY is the one credential, so a remote with none is refused naming it alone. (SHARD-503)
func TestAHostWithNoKeyIsRefused(t *testing.T) {
	var out bytes.Buffer

	app, f, _ := newFrontApp(t, &out)
	noRemoteEnv(t)

	err := app.Run(t.Context(), []string{"--remote", f.url, "list"})
	if err == nil {
		t.Fatal("a host with no key answered")
	}
	if msg := err.Error(); !strings.Contains(msg, client.APIKeyEnv) || strings.Contains(msg, "token-file") || strings.Contains(msg, "SHARD_TOKEN_FILE") {
		t.Errorf("a host with no key returned %q, want %s alone", msg, client.APIKeyEnv)
	}
}

// A script exports the front and the raw key and nothing else, and the front logs the subject, never the key. (SHARD-464)
func TestTheAPIKeyAloneReachesTheFront(t *testing.T) {
	var out bytes.Buffer
	var frontLog syncBuffer

	app, f, _ := newLoggedFrontApp(t, &out, &frontLog, true)
	f.use(t)
	t.Setenv(client.RemoteEnv, f.url)

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list with SHARD_API_KEY alone: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list with SHARD_API_KEY printed %q, want the sandbox the daemon holds", out.String())
	}
	if logged := frontLog.String(); !strings.Contains(logged, "authorized") || strings.Contains(logged, f.key) {
		t.Errorf("the front logged %q, want the authorization and never the key", logged)
	}
}

// A wrong, revoked or expired key reaches the front and gets its 401. (SHARD-464)
func TestAKeyTheFrontDoesNotHonourIsRefusedByTheFront(t *testing.T) {
	var out bytes.Buffer
	var frontLog syncBuffer

	app, f, secret := newLoggedFrontApp(t, &out, &frontLog, true)
	ledger := serve.TokensPath(secret)
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
			f.use(t)
			t.Setenv(client.RemoteEnv, f.url)
			t.Setenv(client.APIKeyEnv, key)

			err := app.Run(t.Context(), []string{"list"})
			if err == nil || !strings.Contains(err.Error(), "did not accept the API key in "+client.APIKeyEnv) || strings.Contains(err.Error(), key) {
				t.Errorf("list with %s returned %v, want the refusal of the front and never the key", name, err)
			}
			if strings.Contains(frontLog.String(), key) {
				t.Errorf("the front logged the key: %q", frontLog.String())
			}
		})
	}
}

func TestServeRefusesAnArgument(t *testing.T) {
	app := App{Version: "test", Root: t.TempDir(), Out: io.Discard}

	if err := app.serve(t.Context(), []string{"127.0.0.1:7850"}); err == nil {
		t.Error("serve took an argument")
	}
}

// noRemoteEnv unsets every variable a remote verb reads, so the shell that runs the test decides nothing.
func noRemoteEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{client.RemoteEnv, client.APIKeyEnv, client.CAFileEnv} {
		t.Setenv(name, "")
	}
}

// accepts is a listener that only counts connections, for a verb that must fail before it dials.
type accepts struct {
	address  string
	accepted chan struct{}
}

func acceptCount(t *testing.T) accepts {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	a := accepts{address: listener.Addr().String(), accepted: make(chan struct{}, 8)}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			a.accepted <- struct{}{}
			conn.Close()
		}
	}()

	return a
}

// none fails the test if anything dialed the listener.
func (a accepts) none(t *testing.T) {
	t.Helper()

	select {
	case <-a.accepted:
		t.Error("the verb dialed the remote, want no connection at all")
	case <-time.After(100 * time.Millisecond):
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

// A front refuses every local route and a host verb would report this host as the server, so either fails under --remote before it dials or touches the root. (SHARD-488, SHARD-598)
func TestALocalOnlyVerbUnderARemoteFailsBeforeItDials(t *testing.T) {
	accepted := acceptCount(t)

	noRemoteEnv(t)
	remote := "https://" + accepted.address
	for verb, args := range map[string][]string{
		"pull":          {"pull", "alpine:3.20"},
		"image list":    {"image", "list"},
		"image remove":  {"image", "remove", "alpine:3.20"},
		"image prune":   {"image", "prune"},
		"daemon status": {"daemon", "status"},
		"daemon":        {"daemon"},
		"serve":         {"serve", "--listen", "127.0.0.1:0"},
		"info":          {"info"},
		"tokens mint":   {"tokens", "mint", "--name", "ci"},
		"tokens list":   {"tokens", "list"},
		"tokens revoke": {"tokens", "revoke", "0123456789abcdef"},
	} {
		root := t.TempDir()
		app := App{Version: "test", Root: root, Remote: remote, Out: io.Discard}
		// A regression runs the daemon or the front, so the deadline ends it rather than the test.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := app.Run(ctx, args)
		cancel()
		if want := "shard " + verb + " runs on the daemon host only"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s under --remote returned %v, want %q", verb, err, want)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read the root: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("%s under --remote left %d entries in the root, want none", verb, len(entries))
		}
	}

	accepted.none(t)
}
