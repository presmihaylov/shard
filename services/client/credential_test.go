package client_test

import (
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
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
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"the bearer token is missing or invalid; send a valid token in Authorization: Bearer TOKEN"}}`))

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

			c, err := client.NewRemoteFromEnv("", client.Config{})
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

			_, err := client.NewRemoteFromEnv("https://shard.example.com", client.Config{})
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
	c, err := client.NewRemoteFromEnv(host, client.Config{})
	if err != nil {
		t.Fatalf("NewRemoteFromEnv: %v", err)
	}
	if _, err := c.Version(t.Context()); err == nil || !strings.Contains(err.Error(), "is not trusted") {
		t.Errorf("Version against a private CA with no %s returned %v, want an untrusted certificate", client.CAFileEnv, err)
	}

	t.Setenv(client.CAFileEnv, ca)
	c, err = client.NewRemoteFromEnv(host, client.Config{})
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

			_, err := client.NewRemoteFromEnv(host, client.Config{})
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

	_, err := client.NewRemoteFromEnv("", client.Config{})
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

			_, err := client.NewRemoteFromEnv("", client.Config{})
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

			c, err := client.NewRemoteFromEnv("", client.Config{})
			if err != nil {
				t.Fatalf("NewRemoteFromEnv: %v", err)
			}
			_, err = c.Version(t.Context())
			if err == nil || strings.Contains(err.Error(), leakKey) || !strings.Contains(err.Error(), "did not accept the API key in "+client.APIKeyEnv+"; set "+client.APIKeyEnv+" to a valid key") {
				t.Errorf("Version with the wrong key returned %v, want the key's source and fix, and never the key", err)
			}
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "bearer") {
				t.Errorf("Version with the wrong key returned %v, which names the wire header", err)
			}

			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
				if printed := fmt.Sprintf(verb, c) + fmt.Sprintf(verb, *c); strings.Contains(printed, leakKey) {
					t.Errorf("%s of the client printed the key: %s", verb, printed)
				}
			}
		})
	}
}

// A saved key the server refuses names the file it came from and how to replace it, never the key or the header. (SHARD-672)
func TestARefusedSavedKeyNamesItsFile(t *testing.T) {
	host, _ := plainFront(t, "key-token")
	noRemoteEnv(t)
	config := t.TempDir()
	t.Setenv(client.ConfigHomeEnv, config)

	c, err := client.NewRemoteFromEnv("", client.Config{Remote: host, APIKey: leakKey})
	if err != nil {
		t.Fatalf("NewRemoteFromEnv: %v", err)
	}
	_, err = c.Version(t.Context())

	want := "the server at " + host + " did not accept the API key saved in " + filepath.Join(config, "shard", "config.json") + "; run shard setup to replace it"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Version with a refused saved key returned %v, want %q in it", err, want)
	}
	if err != nil && (strings.Contains(err.Error(), leakKey) || strings.Contains(strings.ToLower(err.Error()), "bearer")) {
		t.Errorf("Version with a refused saved key returned %v, which holds the key or the header", err)
	}
}

// A remote that does not answer names the cause and the fix, and never a local daemon to check. (SHARD-672)
func TestARemoteThatDoesNotAnswerNamesTheCause(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close the listener: %v", err)
	}
	c, err := client.NewRemote(host, "key-token", nil)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}

	_, err = c.Version(t.Context())

	want := "cannot connect to the shard server at " + host + ": connection refused; check the URL, and that shard serve or the proxy in front of it runs"
	if err == nil || err.Error() != want {
		t.Errorf("Version against a closed port returned %v, want %q", err, want)
	}
}

// sawRequest says whether the front saw a request, without waiting for one.
func sawRequest(seen chan string) bool {
	select {
	case <-seen:
		return true
	default:
		return false
	}
}

// The remote is --remote, else SHARD_REMOTE, else the saved one, and only the chosen server sees a request. (SHARD-657)
func TestTheRemoteIsTheFlagThenSHARDREMOTEThenTheSavedOne(t *testing.T) {
	flag, flagSeen := plainFront(t, "key-token")
	envRemote, envSeen := plainFront(t, "key-token")
	saved, savedSeen := plainFront(t, "key-token")

	for _, tc := range []struct {
		name, host, env string
		want            chan string
	}{
		{name: "the flag", host: flag, env: envRemote, want: flagSeen},
		{name: "SHARD_REMOTE", env: envRemote, want: envSeen},
		{name: "the saved remote", want: savedSeen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, tc.env)
			t.Setenv(client.APIKeyEnv, "key-token")

			c, err := client.NewRemoteFromEnv(tc.host, client.Config{Remote: saved, APIKey: "key-token"})
			if err != nil {
				t.Fatalf("NewRemoteFromEnv: %v", err)
			}
			if _, err := c.Version(t.Context()); err != nil {
				t.Fatalf("Version: %v", err)
			}
			for name, seen := range map[string]chan string{"the flag": flagSeen, "SHARD_REMOTE": envSeen, "the saved remote": savedSeen} {
				if got := sawRequest(seen); got != (seen == tc.want) {
					t.Errorf("the front of %s saw a request: %v", name, got)
				}
			}
		})
	}
}

// SHARD_API_KEY beats the saved key, and an empty or blank one counts as unset, so the saved key rides. (SHARD-657)
func TestSHARDAPIKEYBeatsTheSavedKey(t *testing.T) {
	for _, tc := range []struct {
		name, env, want string
	}{
		{name: "set", env: "env-key", want: "env-key"},
		{name: "unset", env: "", want: "saved-key"},
		{name: "blank", env: " \t\n", want: "saved-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, seen := plainFront(t, tc.want)
			noRemoteEnv(t)
			t.Setenv(client.APIKeyEnv, tc.env)

			c, err := client.NewRemoteFromEnv("", client.Config{Remote: host, APIKey: "saved-key"})
			if err != nil {
				t.Fatalf("NewRemoteFromEnv: %v", err)
			}
			if _, err := c.Version(t.Context()); err != nil {
				t.Fatalf("Version: %v", err)
			}
			if got := <-seen; got != "Bearer "+tc.want {
				t.Errorf("the front saw %q, want %q as the bearer", got, tc.want)
			}
		})
	}
}

// The saved key goes to the server it was saved for alone; another remote needs SHARD_API_KEY, and the refusal never quotes the key. (SHARD-657)
func TestTheSavedKeyGoesToTheSavedRemoteAlone(t *testing.T) {
	other, otherSeen := plainFront(t, leakKey)
	saved, _ := plainFront(t, leakKey)

	for name, set := range map[string]func(*testing.T) string{
		"the flag":     func(*testing.T) string { return other },
		"SHARD_REMOTE": func(t *testing.T) string { t.Setenv(client.RemoteEnv, other); return "" },
	} {
		t.Run(name, func(t *testing.T) {
			noRemoteEnv(t)
			host := set(t)

			_, err := client.NewRemoteFromEnv(host, client.Config{Remote: saved, APIKey: leakKey})
			if err == nil {
				t.Fatal("NewRemoteFromEnv sent the saved key to another server")
			}
			if msg := err.Error(); !strings.Contains(msg, client.APIKeyEnv) || !strings.Contains(msg, saved) || strings.Contains(msg, leakKey) {
				t.Errorf("NewRemoteFromEnv returned %q, want %s and the saved remote in it and never the key", msg, client.APIKeyEnv)
			}
			if sawRequest(otherSeen) {
				t.Error("the other server saw a request")
			}
		})
	}
}

// One server is one scheme and one host and port: the case of the name, a path and the default port make no other, a scheme or a port does. (SHARD-657)
func TestTheSavedKeyMatchesItsServerWhateverTheSpelling(t *testing.T) {
	saved := client.Config{Remote: "https://Shard.Example.com/", APIKey: leakKey}

	for host, same := range map[string]bool{
		"https://shard.example.com":         true,
		"https://shard.example.com:443/v0":  true,
		"http://shard.example.com":          false,
		"https://shard.example.com:8443":    false,
		"https://shard.example.com.evil.io": false,
	} {
		t.Run(host, func(t *testing.T) {
			noRemoteEnv(t)

			_, err := client.NewRemoteFromEnv(host, saved)
			if same && err != nil {
				t.Errorf("NewRemoteFromEnv refused the saved server: %v", err)
			}
			if !same && err == nil {
				t.Error("NewRemoteFromEnv sent the saved key to another server")
			}
		})
	}
}

// A refusal names the saved remote with its password hidden, as url.URL.Redacted prints it. (SHARD-657)
func TestARefusalHidesThePasswordOfTheSavedRemote(t *testing.T) {
	noRemoteEnv(t)

	_, err := client.NewRemoteFromEnv("https://other.example.com", client.Config{Remote: "https://user:" + leakKey + "@shard.example.com", APIKey: "saved-key"})
	if err == nil || strings.Contains(err.Error(), leakKey) || !strings.Contains(err.Error(), "shard.example.com") {
		t.Errorf("NewRemoteFromEnv returned %v, want the saved remote in it and never its password", err)
	}
}

// Reach dials and shakes hands and sends no request, so a server that refuses the key still answers it, and an untrusted certificate fails it. (SHARD-657)
func TestReachDialsWithoutARequest(t *testing.T) {
	host, ca, seen := tlsFront(t, "key-token")
	caBytes, err := client.ReadCA(host, ca)
	if err != nil {
		t.Fatalf("ReadCA: %v", err)
	}

	c, err := client.NewRemote(host, "not-the-key", caBytes)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	if err := c.Reach(t.Context()); err != nil {
		t.Errorf("Reach with a key the front refuses: %v", err)
	}
	if sawRequest(seen) {
		t.Error("Reach sent a request")
	}

	c, err = client.NewRemote(host, "key-token", nil)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	var untrusted *tls.CertificateVerificationError
	if err := c.Reach(t.Context()); !errors.As(err, &untrusted) {
		t.Errorf("Reach against a private CA with none returned %v, want an untrusted certificate", err)
	}

	listener := httptest.NewServer(http.NotFoundHandler())
	listener.Close()
	c, err = client.NewRemote(listener.URL, "key-token", nil)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	var connect *client.ConnectError
	if err := c.Reach(t.Context()); !errors.As(err, &connect) {
		t.Errorf("Reach of a closed port returned %v, want a ConnectError", err)
	}
}

// No SHARD_CA_FILE is no certificate and no error. (SHARD-657)
func TestReadCAWithNoFileIsNone(t *testing.T) {
	ca, err := client.ReadCA("https://shard.example.com", "")
	if err != nil || ca != nil {
		t.Errorf("ReadCA with no file answered %d bytes and %v, want none", len(ca), err)
	}
}
