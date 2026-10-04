package client_test

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/client"
)

// leakKey is a synthetic key no real credential holds, so any text that carries it is a leak.
const leakKey = "shard464-synthetic-key-0f9e8d7c"

// noRemoteEnv unsets every variable a remote client reads, so the shell that runs the test decides nothing.
func noRemoteEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{client.RemoteEnv, client.APIKeyEnv, client.CAFileEnv} {
		t.Setenv(name, "")
	}
}

// tlsFront answers the version to the token accepted alone and a 401 to any other, and hands back its url, its CA file and each header it saw.
func tlsFront(t *testing.T, accepted string) (string, string, chan string) {
	t.Helper()

	server, seen := newFront(accepted)
	server.StartTLS()
	t.Cleanup(server.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write the ca file: %v", err)
	}

	return server.URL, ca, seen
}

// plainFront is tlsFront over http, as shard serve on loopback answers, and has no CA to hand back.
func plainFront(t *testing.T, accepted string) (string, chan string) {
	t.Helper()

	server, seen := newFront(accepted)
	server.Start()
	t.Cleanup(server.Close)

	return server.URL, seen
}

func newFront(accepted string) (*httptest.Server, chan string) {
	seen := make(chan string, 8)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+accepted {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"the request carries no valid bearer token"}}`))

			return
		}
		_, _ = w.Write([]byte(`{"version":"test"}`))
	}))

	return server, seen
}

// SHARD_API_KEY is the one credential, and it rides as the bearer over http and https alike. (SHARD-503)
func TestNewRemoteFromEnvSendsTheKeyOverEitherScheme(t *testing.T) {
	secure, ca, secureSeen := tlsFront(t, "key-token")
	plain, plainSeen := plainFront(t, "key-token")

	for _, tc := range []struct {
		name, host, ca string
		seen           chan string
		plain          bool
	}{
		{name: "https", host: secure, ca: ca, seen: secureSeen},
		{name: "http", host: plain, seen: plainSeen, plain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, tc.host)
			t.Setenv(client.APIKeyEnv, "key-token")
			t.Setenv(client.CAFileEnv, tc.ca)

			c, err := client.NewRemoteFromEnv("")
			if err != nil {
				t.Fatalf("NewRemoteFromEnv: %v", err)
			}
			if c.Plain() != tc.plain {
				t.Errorf("Plain answered %v for %s", c.Plain(), tc.host)
			}
			if _, err := c.Version(t.Context()); err != nil {
				t.Fatalf("Version: %v", err)
			}
			if got := <-tc.seen; got != "Bearer key-token" {
				t.Errorf("the front saw %q, want the key as the bearer", got)
			}
		})
	}
}

// With SHARD_API_KEY unset or blank the refusal names it and no other source, as no other is read. (SHARD-503)
func TestAMissingKeyNamesSHARDAPIKEYAlone(t *testing.T) {
	for name, key := range map[string]string{"unset": "", "blank": " \t\n"} {
		t.Run(name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.APIKeyEnv, key)

			_, err := client.NewRemoteFromEnv("https://shard.example.com")
			if err == nil {
				t.Fatal("NewRemoteFromEnv answered with no key")
			}
			if msg := err.Error(); !strings.Contains(msg, client.APIKeyEnv) || strings.Contains(msg, "token-file") || strings.Contains(msg, "SHARD_TOKEN_FILE") {
				t.Errorf("a missing key returned %q, want %s alone", msg, client.APIKeyEnv)
			}
		})
	}
}

// An https remote keeps verification: the host's trust store refuses a private CA, and SHARD_CA_FILE trusts it. (SHARD-503)
func TestHTTPSTrustsTheStoreOrSHARDCAFILE(t *testing.T) {
	host, ca, _ := tlsFront(t, "key-token")

	noRemoteEnv(t)
	t.Setenv(client.APIKeyEnv, "key-token")
	c, err := client.NewRemoteFromEnv(host)
	if err != nil {
		t.Fatalf("NewRemoteFromEnv: %v", err)
	}
	if _, err := c.Version(t.Context()); err == nil || !strings.Contains(err.Error(), "is not trusted") {
		t.Errorf("Version against a private CA with no %s returned %v, want an untrusted certificate", client.CAFileEnv, err)
	}

	t.Setenv(client.CAFileEnv, ca)
	c, err = client.NewRemoteFromEnv(host)
	if err != nil {
		t.Fatalf("NewRemoteFromEnv: %v", err)
	}
	if _, err := c.Version(t.Context()); err != nil {
		t.Errorf("Version with %s: %v", client.CAFileEnv, err)
	}
}

// SHARD_CA_FILE verifies a certificate an http remote never shows, so the pair is refused before any dial or read of the file. (SHARD-503)
func TestSHARDCAFILEWithAnHTTPRemoteIsRefused(t *testing.T) {
	host, seen := plainFront(t, "key-token")
	_, ca, _ := tlsFront(t, "key-token")

	for name, caFile := range map[string]string{"a ca file": ca, "a missing ca file": filepath.Join(t.TempDir(), "missing.pem")} {
		t.Run(name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.APIKeyEnv, "key-token")
			t.Setenv(client.CAFileEnv, caFile)

			_, err := client.NewRemoteFromEnv(host)
			if err == nil || !strings.Contains(err.Error(), client.CAFileEnv) || !strings.Contains(err.Error(), host) {
				t.Errorf("%s with %s returned %v, want a refusal that names both", client.CAFileEnv, host, err)
			}
		})
	}

	if _, err := client.NewRemote(host, "key-token", []byte("any certificate")); err == nil || !strings.Contains(err.Error(), host) {
		t.Errorf("NewRemote with a ca and %s returned %v, want a refusal", host, err)
	}
	select {
	case got := <-seen:
		t.Errorf("the front saw a request carrying %q, want none", got)
	default:
	}
}

func TestNewRemoteFromEnvNeedsAHost(t *testing.T) {
	noRemoteEnv(t)
	t.Setenv(client.APIKeyEnv, "key-token")

	_, err := client.NewRemoteFromEnv("")
	if err == nil || !strings.Contains(err.Error(), client.RemoteEnv) {
		t.Errorf("NewRemoteFromEnv with no host returned %v, want a refusal that names %s", err, client.RemoteEnv)
	}
}

// No refusal on the way to a front, nor a print of the client, holds the key; each names where the key came from instead.
func TestNoErrorHoldsTheKey(t *testing.T) {
	host, ca, _ := tlsFront(t, "the-one-valid-token")
	noCert := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(noCert, []byte("no certificate here\n"), 0o600); err != nil {
		t.Fatalf("write the ca file: %v", err)
	}

	for _, tc := range []struct {
		name     string
		host     string
		key      string
		caFile   string
		mentions string
	}{
		{name: "a newline in the key", host: host, key: leakKey + "\n" + leakKey, caFile: ca, mentions: client.APIKeyEnv},
		{name: "a carriage return in the key", host: host, key: leakKey + "\r\nX-Injected: 1", caFile: ca, mentions: client.APIKeyEnv},
		{name: "a delete byte in the key", host: host, key: leakKey + "\x7f", caFile: ca, mentions: client.APIKeyEnv},
		{name: "a remote of another scheme", host: "ftp://box.example.com", key: leakKey, caFile: ca, mentions: "http or https"},
		{name: "an http remote with a ca file", host: "http://box.example.com", key: leakKey, caFile: ca, mentions: client.CAFileEnv},
		{name: "a remote that does not parse", host: "https://[::1", key: leakKey, caFile: ca, mentions: "parse"},
		{name: "a missing ca file", host: host, key: leakKey, caFile: filepath.Join(t.TempDir(), "missing.pem"), mentions: "ca file"},
		{name: "a ca file of no certificate", host: host, key: leakKey, caFile: noCert, mentions: "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, tc.host)
			t.Setenv(client.APIKeyEnv, tc.key)
			t.Setenv(client.CAFileEnv, tc.caFile)

			_, err := client.NewRemoteFromEnv("")
			if err == nil {
				t.Fatal("NewRemoteFromEnv accepted it")
			}
			if strings.Contains(err.Error(), leakKey) || !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("NewRemoteFromEnv returned %q, want %q in it and never the key", err, tc.mentions)
			}
		})
	}

	t.Run("a raw key with a newline handed to NewRemote", func(t *testing.T) {
		_, err := client.NewRemote(host, leakKey+"\n"+leakKey, nil)
		if err == nil || strings.Contains(err.Error(), leakKey) {
			t.Errorf("NewRemote returned %v, want a refusal that never holds the key", err)
		}
	})

	// A key of any shape a header carries reaches the front, so a wrong one gets the 401 of serve and not a guess of the client.
	for name, key := range map[string]string{
		"a wrong key":                leakKey,
		"a space in the key":         leakKey + " " + leakKey,
		"a tab in the key":           leakKey + "\t" + leakKey,
		"a byte past ascii in a key": leakKey + "\xff",
		"a whole mint record":        `{"token":"` + leakKey + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, host)
			t.Setenv(client.APIKeyEnv, key)
			t.Setenv(client.CAFileEnv, ca)

			c, err := client.NewRemoteFromEnv("")
			if err != nil {
				t.Fatalf("NewRemoteFromEnv: %v", err)
			}
			_, err = c.Version(t.Context())
			if err == nil || strings.Contains(err.Error(), leakKey) || !strings.Contains(err.Error(), "no valid bearer token") {
				t.Errorf("Version with the wrong key returned %v, want the refusal of the front and never the key", err)
			}

			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
				if printed := fmt.Sprintf(verb, c) + fmt.Sprintf(verb, *c); strings.Contains(printed, leakKey) {
					t.Errorf("%s of the client printed the key: %s", verb, printed)
				}
			}
		})
	}
}
