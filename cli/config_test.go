package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/client"
)

// leakKey is a synthetic key no real credential holds, so any text that carries it is a leak.
const leakKey = "shard657-synthetic-key-5a4b3c2d"

// isolateConfig points the saved connection at an empty directory of its own, so no test reads or writes the user's.
func isolateConfig() (func() error, error) {
	dir, err := os.MkdirTemp("", "shard-config")
	if err != nil {
		return nil, fmt.Errorf("make a configuration directory: %w", err)
	}
	if err := os.Setenv(client.ConfigHomeEnv, dir); err != nil {
		return nil, fmt.Errorf("set %s: %w", client.ConfigHomeEnv, err)
	}

	return func() error { return os.RemoveAll(dir) }, nil
}

// saveConnection saves remote and key as shard setup would, under a configuration directory of the test's own.
func saveConnection(t *testing.T, remote, key string) {
	t.Helper()

	t.Setenv(client.ConfigHomeEnv, t.TempDir())
	path, err := client.ConfigPath(os.Getenv)
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if err := client.SaveConfig(path, client.Config{Remote: remote, APIKey: key}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

// A saved connection names the server with no flag and no SHARD_REMOTE or SHARD_API_KEY, and the verb goes there, not to the socket. (SHARD-657)
func TestASavedConnectionReachesTheFront(t *testing.T) {
	var out bytes.Buffer

	app, f, _ := newFrontApp(t, &out)
	noRemoteEnv(t)
	t.Setenv(client.CAFileEnv, f.ca)
	saveConnection(t, f.url, f.key)
	// The front still serves the daemon of the old root; the CLI gets a root with no socket at all.
	app.Root = t.TempDir()

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list through the saved connection: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list through the saved connection printed %q, want the sandbox the daemon holds", out.String())
	}
}

// An explicit empty --remote asks for the socket, so the saved server sees no dial and the saved file is not read. (SHARD-657)
func TestAnEmptyRemoteFlagIgnoresTheSavedConnection(t *testing.T) {
	var out bytes.Buffer
	accepted := acceptCount(t)

	app := newListApp(t, &out, listed(), nil)
	noRemoteEnv(t)
	saveConnection(t, "https://"+accepted.address, leakKey)

	if err := app.Run(t.Context(), []string{"--remote", "", "list"}); err != nil {
		t.Fatalf("list over the socket: %v", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list over the socket printed %q, want the sandbox the daemon holds", out.String())
	}
	accepted.none(t)
}

// A host verb would report this host as the saved server, so it refuses before it dials or touches the root, and never prints the key. (SHARD-657)
func TestAHostVerbRefusesASavedConnection(t *testing.T) {
	accepted := acceptCount(t)

	noRemoteEnv(t)
	saveConnection(t, "https://"+accepted.address, leakKey)
	for verb, args := range map[string][]string{
		"pull":          {"pull", "alpine:3.20"},
		"image list":    {"image", "list"},
		"daemon status": {"daemon", "status"},
		"info":          {"info"},
		"tokens mint":   {"tokens", "mint", "--name", "ci"},
		"tokens list":   {"tokens", "list"},
	} {
		root := t.TempDir()
		app := App{Version: "test", Root: root, Out: io.Discard}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := app.Run(ctx, args)
		cancel()
		if want := "shard " + verb + " runs on the daemon host only"; err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "saved connection") {
			t.Errorf("%s with a saved connection returned %v, want %q and the saved connection", verb, err, want)
		}
		if err != nil && strings.Contains(err.Error(), leakKey) {
			t.Errorf("%s printed the saved key", verb)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read the root: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("%s with a saved connection left %d entries in the root, want none", verb, len(entries))
		}
	}

	accepted.none(t)
}

// The saved connection says where commands go, never where a front runs, so serve fails for its own reason alone. (SHARD-657)
func TestServeIgnoresTheSavedConnection(t *testing.T) {
	accepted := acceptCount(t)

	noRemoteEnv(t)
	saveConnection(t, "https://"+accepted.address, leakKey)
	missing := filepath.Join(t.TempDir(), "missing-signing-key")
	app := App{Version: "test", Root: t.TempDir(), Out: io.Discard}

	err := app.Run(t.Context(), []string{"serve", "--listen", "127.0.0.1:0", "--signing-key-file", missing})
	if err == nil || strings.Contains(err.Error(), "daemon host only") || !strings.Contains(err.Error(), missing) {
		t.Errorf("serve with a saved connection returned %v, want the missing signing key", err)
	}
	accepted.none(t)
}

// A saved file no client can use stops every verb with its path, rather than a quiet fall back to the socket. (SHARD-657)
func TestABrokenSavedConnectionNamesTheFile(t *testing.T) {
	var out bytes.Buffer

	app := newListApp(t, &out, listed(), nil)
	noRemoteEnv(t)
	t.Setenv(client.ConfigHomeEnv, t.TempDir())
	path, err := client.ConfigPath(os.Getenv)
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("make the configuration directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"remote":"ftp://shard.example.com","api_key":"`+leakKey+`"}`), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}

	err = app.Run(t.Context(), []string{"list"})
	if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), leakKey) {
		t.Errorf("list with a broken saved connection returned %v, want its path and never the key", err)
	}
	if strings.Contains(out.String(), "up-1") {
		t.Error("list with a broken saved connection read the socket")
	}
}

// bearerFront is a plain http server that only records the bearer of each request, so a row can tell which server a verb dialed.
func bearerFront(t *testing.T) (string, func() []string) {
	t.Helper()

	var mu sync.Mutex
	var bearers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		http.Error(w, `{"error":"recorded"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(bearers)
	}
}

// The §13 order: --remote, then SHARD_REMOTE, then the saved connection, then the socket; an explicit empty --remote is the socket. (SHARD-657)
func TestTheRemoteComesFromTheFlagThenTheEnvironmentThenTheSavedConnection(t *testing.T) {
	const envKey, savedKey = "shard657-synthetic-env-key", "shard657-synthetic-saved-key"
	for _, tc := range []struct {
		name                   string
		flag, env, saved, key  bool
		emptyFlag              bool
		wantServer, wantBearer string
	}{
		{name: "flag over env and saved", flag: true, env: true, saved: true, key: true, wantServer: "flag", wantBearer: envKey},
		{name: "flag over saved", flag: true, saved: true, key: true, wantServer: "flag", wantBearer: envKey},
		{name: "env over saved", env: true, saved: true, key: true, wantServer: "env", wantBearer: envKey},
		{name: "env alone", env: true, key: true, wantServer: "env", wantBearer: envKey},
		{name: "saved alone, with its key", saved: true, wantServer: "saved", wantBearer: savedKey},
		{name: "SHARD_API_KEY over the saved key", saved: true, key: true, wantServer: "saved", wantBearer: envKey},
		{name: "empty flag over env and saved", emptyFlag: true, env: true, saved: true, key: true},
		{name: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			app := newListApp(t, &out, listed(), nil)
			noRemoteEnv(t)
			t.Setenv(client.ConfigHomeEnv, t.TempDir())

			urls := map[string]string{}
			bearers := map[string]func() []string{}
			for _, name := range []string{"flag", "env", "saved"} {
				urls[name], bearers[name] = bearerFront(t)
			}
			if tc.saved {
				saveConnection(t, urls["saved"], savedKey)
			}
			if tc.env {
				t.Setenv(client.RemoteEnv, urls["env"])
			}
			if tc.key {
				t.Setenv(client.APIKeyEnv, envKey)
			}
			args := []string{"list"}
			if tc.flag {
				args = append([]string{"--remote", urls["flag"]}, args...)
			}
			if tc.emptyFlag {
				args = append([]string{"--remote", ""}, args...)
			}

			err := app.Run(t.Context(), args)
			for name, got := range bearers {
				if name != tc.wantServer && len(got()) != 0 {
					t.Errorf("the %s server got %d requests, want none", name, len(got()))
				}
			}
			if tc.wantServer == "" {
				if err != nil || !strings.Contains(out.String(), "up-1") {
					t.Errorf("list returned %v and printed %q, want the sandbox the socket holds", err, out.String())
				}
				return
			}
			got := bearers[tc.wantServer]()
			if len(got) == 0 || got[0] != tc.wantBearer {
				t.Errorf("the %s server got bearers %q, want %q", tc.wantServer, got, tc.wantBearer)
			}
			if strings.Contains(out.String(), "up-1") {
				t.Error("list read the socket, want the remote")
			}
		})
	}
}
