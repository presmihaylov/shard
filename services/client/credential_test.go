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

	for _, name := range []string{client.RemoteEnv, client.APIKeyEnv, client.TokenFileEnv, client.CAFileEnv} {
		t.Setenv(name, "")
	}
}

func writeTokenFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}

	return path
}

// tlsFront answers the version to the token accepted alone and a 401 to any other, and hands back its url, its CA file and each header it saw.
func tlsFront(t *testing.T, accepted string) (string, string, chan string) {
	t.Helper()

	seen := make(chan string, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+accepted {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"the request carries no valid bearer token"}}`))

			return
		}
		_, _ = w.Write([]byte(`{"version":"test"}`))
	}))
	t.Cleanup(server.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatalf("write the ca file: %v", err)
	}

	return server.URL, ca, seen
}

// The token comes from the first source set: the explicit file, then SHARD_API_KEY, then SHARD_TOKEN_FILE. (SHARD-464)
func TestResolveTokenTakesTheFirstSourceSet(t *testing.T) {
	flagFile := writeTokenFile(t, "flag-token\n")
	envFile := writeTokenFile(t, `{"token":"env-token","expires_at":null,"scopes":["*"]}`)

	for _, tc := range []struct {
		name               string
		flag, key, envFile string
		want               string
	}{
		{name: "all three", flag: flagFile, key: "key-token", envFile: envFile, want: "flag-token"},
		{name: "the flag and the key", flag: flagFile, key: "key-token", want: "flag-token"},
		{name: "the flag and the env file", flag: flagFile, envFile: envFile, want: "flag-token"},
		{name: "the key and the env file", key: "key-token", envFile: envFile, want: "key-token"},
		{name: "the flag alone", flag: flagFile, want: "flag-token"},
		{name: "the key alone", key: "key-token", want: "key-token"},
		{name: "the env file alone", envFile: envFile, want: "env-token"},
		{name: "a key with whitespace around it", key: " \tkey-token\n", envFile: envFile, want: "key-token"},
		{name: "an empty key", key: "", envFile: envFile, want: "env-token"},
		{name: "a blank key", key: " \t\n ", envFile: envFile, want: "env-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.APIKeyEnv, tc.key)
			t.Setenv(client.TokenFileEnv, tc.envFile)

			got, err := client.ResolveToken(tc.flag)
			if err != nil {
				t.Fatalf("ResolveToken: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveToken answered %q, want %q", got, tc.want)
			}
		})
	}
}

// With no source set, and an empty SHARD_TOKEN_FILE is none, the refusal names all three in the order they win.
func TestResolveTokenWithNoSourceNamesAllThreeInOrder(t *testing.T) {
	noRemoteEnv(t)
	t.Setenv(client.APIKeyEnv, "  ")

	_, err := client.ResolveToken("")
	if err == nil {
		t.Fatal("ResolveToken answered with no source set")
	}

	msg := err.Error()
	flag, key, file := strings.Index(msg, "--token-file"), strings.Index(msg, client.APIKeyEnv), strings.Index(msg, client.TokenFileEnv)
	if flag < 0 || key < flag || file < key {
		t.Errorf("ResolveToken returned %q, want --token-file, %s and %s in that order", msg, client.APIKeyEnv, client.TokenFileEnv)
	}
}

// A token file takes the mint record whole or a bare token, and refuses a broken record.
func TestATokenFileTakesTheRecordOrTheBareToken(t *testing.T) {
	noRemoteEnv(t)

	for name, body := range map[string]string{
		"a bare token": "  the.jwt.value\n",
		"a record":     `{"token":"the.jwt.value","expires_at":null,"scopes":["*"]}` + "\n",
	} {
		got, err := client.ResolveToken(writeTokenFile(t, body))
		if err != nil {
			t.Fatalf("%s: ResolveToken: %v", name, err)
		}
		if got != "the.jwt.value" {
			t.Errorf("%s: ResolveToken answered %q, want the token", name, got)
		}
	}

	for name, body := range map[string]string{
		"a broken record":      "{not json",
		"a record of no token": `{"scopes":["*"]}`,
		"an empty file":        "\n",
	} {
		if _, err := client.ResolveToken(writeTokenFile(t, body)); err == nil {
			t.Errorf("ResolveToken accepted %s", name)
		}
	}
}

func TestATokenFileTheHostCanReadIsRefused(t *testing.T) {
	noRemoteEnv(t)

	path := writeTokenFile(t, "cli-token")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := client.ResolveToken(path); err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
}

// The SDK builds the client the CLI does, from the same environment in the same order, and sends the token as the bearer.
func TestNewRemoteFromEnvSendsTheTokenTheOrderPicks(t *testing.T) {
	host, ca, seen := tlsFront(t, "")
	flagFile := writeTokenFile(t, "flag-token")
	envFile := writeTokenFile(t, "env-token")

	for _, tc := range []struct {
		name               string
		flag, key, envFile string
		want               string
	}{
		{name: "the key alone", key: "key-token", want: "key-token"},
		{name: "the key over the env file", key: "key-token", envFile: envFile, want: "key-token"},
		{name: "the flag over the key", flag: flagFile, key: "key-token", envFile: envFile, want: "flag-token"},
		{name: "the env file under a blank key", key: " ", envFile: envFile, want: "env-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, host)
			t.Setenv(client.CAFileEnv, ca)
			t.Setenv(client.APIKeyEnv, tc.key)
			t.Setenv(client.TokenFileEnv, tc.envFile)

			c, err := client.NewRemoteFromEnv(client.RemoteOptions{TokenFile: tc.flag})
			if err != nil {
				t.Fatalf("NewRemoteFromEnv: %v", err)
			}
			// The front refuses every token here, so the answer is a 401 and what matters is the header it saw.
			if _, err := c.Version(t.Context()); err == nil {
				t.Fatal("the front accepted a token it accepts none of")
			}
			if got := <-seen; got != "Bearer "+tc.want {
				t.Errorf("the front saw %q, want the bearer %s", got, tc.want)
			}
		})
	}
}

// The options beat the environment, so a caller that names its front reaches that one.
func TestNewRemoteFromEnvTakesTheOptionsOverTheEnvironment(t *testing.T) {
	host, ca, seen := tlsFront(t, "key-token")
	noRemoteEnv(t)
	t.Setenv(client.RemoteEnv, "https://elsewhere.invalid:2376")
	t.Setenv(client.CAFileEnv, filepath.Join(t.TempDir(), "missing.pem"))
	t.Setenv(client.APIKeyEnv, "key-token")

	c, err := client.NewRemoteFromEnv(client.RemoteOptions{Host: host, CAFile: ca})
	if err != nil {
		t.Fatalf("NewRemoteFromEnv: %v", err)
	}
	if _, err := c.Version(t.Context()); err != nil {
		t.Fatalf("Version through the named front: %v", err)
	}
	if got := <-seen; got != "Bearer key-token" {
		t.Errorf("the front saw %q, want the key", got)
	}
}

func TestNewRemoteFromEnvNeedsAHost(t *testing.T) {
	noRemoteEnv(t)
	t.Setenv(client.APIKeyEnv, "key-token")

	_, err := client.NewRemoteFromEnv(client.RemoteOptions{})
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
		{name: "a plain http remote", host: "http://box.example.com:2376", key: leakKey, caFile: ca, mentions: "https"},
		{name: "a remote that does not parse", host: "https://[::1", key: leakKey, caFile: ca, mentions: "parse"},
		{name: "a missing ca file", host: host, key: leakKey, caFile: filepath.Join(t.TempDir(), "missing.pem"), mentions: "ca file"},
		{name: "a ca file of no certificate", host: host, key: leakKey, caFile: noCert, mentions: "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noRemoteEnv(t)
			t.Setenv(client.RemoteEnv, tc.host)
			t.Setenv(client.APIKeyEnv, tc.key)
			t.Setenv(client.CAFileEnv, tc.caFile)

			_, err := client.NewRemoteFromEnv(client.RemoteOptions{})
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

	t.Run("a token file of two lines", func(t *testing.T) {
		noRemoteEnv(t)
		path := writeTokenFile(t, leakKey+"\n"+leakKey+"\n")

		_, err := client.ResolveToken(path)
		if err == nil || strings.Contains(err.Error(), leakKey) || !strings.Contains(err.Error(), path) {
			t.Errorf("ResolveToken returned %v, want a refusal that names %s and never holds the key", err, path)
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

			c, err := client.NewRemoteFromEnv(client.RemoteOptions{})
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
