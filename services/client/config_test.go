package client_test

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/client"
)

// env answers the variables a test names and the empty string for every other, so the shell that runs the test decides nothing.
func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// The file lives under XDG_CONFIG_HOME, or ~/.config, a relative XDG_CONFIG_HOME is ignored as the XDG spec says, and no HOME asks the account database. (SHARD-657)
func TestConfigPathFollowsTheXDGSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{name: "xdg", vars: map[string]string{client.ConfigHomeEnv: "/xdg", "HOME": "/home/u"}, want: "/xdg/shard/config.json"},
		{name: "no xdg", vars: map[string]string{"HOME": "/home/u"}, want: "/home/u/.config/shard/config.json"},
		{name: "relative xdg", vars: map[string]string{client.ConfigHomeEnv: "xdg", "HOME": "/home/u"}, want: "/home/u/.config/shard/config.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client.ConfigPath(env(tc.vars))
			if err != nil {
				t.Fatalf("ConfigPath: %v", err)
			}
			if got != tc.want {
				t.Errorf("ConfigPath answered %s, want %s", got, tc.want)
			}
		})
	}

	current, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	got, err := client.ConfigPath(env(nil))
	if err != nil {
		t.Fatalf("ConfigPath with no HOME: %v", err)
	}
	if want := filepath.Join(current.HomeDir, ".config", "shard", "config.json"); got != want {
		t.Errorf("ConfigPath with no HOME answered %s, want %s from the account database", got, want)
	}
}

// A saved connection is the user's alone, the directory 0700 and the file 0600, even where the directory was wider. (SHARD-657)
func TestSaveConfigRestrictsTheFileAndItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shard")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	path := filepath.Join(dir, "config.json")

	saved := client.Config{Remote: "https://shard.example.com", APIKey: leakKey}
	if err := client.SaveConfig(path, saved); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", p, got, want)
		}
	}

	got, err := client.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got != saved {
		t.Errorf("LoadConfig answered a different connection than SaveConfig wrote")
	}
}

// A save replaces the whole file through a rename, and leaves no temp file beside it. (SHARD-657)
func TestSaveConfigReplacesTheWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard", "config.json")

	if err := client.SaveConfig(path, client.Config{Remote: "https://old.example.com", APIKey: "old-key"}); err != nil {
		t.Fatalf("SaveConfig the old connection: %v", err)
	}
	replacement := client.Config{Remote: "http://new.example.com:2376", APIKey: "new-key"}
	if err := client.SaveConfig(path, replacement); err != nil {
		t.Fatalf("SaveConfig the new connection: %v", err)
	}

	got, err := client.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got != replacement {
		t.Errorf("LoadConfig answered %v, want the replacement", got)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries after two saves, want config.json alone", len(entries))
	}
}

// A connection that is incomplete or that no client can use is never written, and the refusal never quotes the key. (SHARD-657)
func TestSaveConfigRefusesAConnectionNoClientCanUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard", "config.json")

	for name, c := range map[string]client.Config{
		"no remote":  {APIKey: leakKey},
		"no key":     {Remote: "https://shard.example.com"},
		"blank key":  {Remote: "https://shard.example.com", APIKey: " \t"},
		"ftp remote": {Remote: "ftp://shard.example.com", APIKey: leakKey},
		"split key":  {Remote: "https://shard.example.com", APIKey: leakKey + "\nX-Other: 1"},
	} {
		t.Run(name, func(t *testing.T) {
			err := client.SaveConfig(path, c)
			if err == nil {
				t.Fatal("SaveConfig wrote the connection")
			}
			if strings.Contains(err.Error(), leakKey) {
				t.Errorf("the refusal %q quotes the key", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("stat %s returned %v after a refused save, want no file", path, err)
			}
		})
	}
}

// No file is no saved connection, and no error: most hosts save none. (SHARD-657)
func TestLoadConfigWithNoFileIsNoConnection(t *testing.T) {
	got, err := client.LoadConfig(filepath.Join(t.TempDir(), "shard", "config.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got != (client.Config{}) {
		t.Errorf("LoadConfig answered %v, want the zero connection", got)
	}
}

// A file the client cannot use is refused with its path, never read in part, and the refusal never quotes the key. (SHARD-657)
func TestLoadConfigRefusesAFileTheClientCannotUse(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      "remote = https://shard.example.com",
		"unknown field": `{"remote":"https://shard.example.com","api_key":"` + leakKey + `","token":"x"}`,
		"ftp remote":    `{"remote":"ftp://shard.example.com","api_key":"` + leakKey + `"}`,
		"split key":     `{"remote":"https://shard.example.com","api_key":"` + leakKey + `\r\nX-Other: 1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write the file: %v", err)
			}

			_, err := client.LoadConfig(path)
			if err == nil {
				t.Fatal("LoadConfig read the file")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal %q does not name the file", err)
			}
			if strings.Contains(err.Error(), leakKey) {
				t.Errorf("the refusal %q quotes the key", err)
			}
		})
	}
}

// Remove deletes the saved connection, and one already gone is not an error. (SHARD-657)
func TestRemoveConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard", "config.json")
	if err := client.SaveConfig(path, client.Config{Remote: "https://shard.example.com", APIKey: leakKey}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	for range 2 {
		if err := client.RemoveConfig(path); err != nil {
			t.Fatalf("RemoveConfig: %v", err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s returned %v after RemoveConfig, want no file", path, err)
	}
}

// A saved connection prints its remote alone, whatever the verb and with its password hidden, so no error or log line shows a secret. (SHARD-657)
func TestAConfigNeverPrintsItsKey(t *testing.T) {
	c := client.Config{Remote: "https://shard.example.com", APIKey: leakKey}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		got := fmt.Sprintf(verb, c)
		if strings.Contains(got, leakKey) {
			t.Errorf("%s printed the key: %s", verb, got)
		}
		if !strings.Contains(got, c.Remote) {
			t.Errorf("%s printed %q, want the remote", verb, got)
		}
	}

	if got := fmt.Sprint(client.Config{Remote: "https://user:" + leakKey + "@shard.example.com", APIKey: "saved-key"}); strings.Contains(got, leakKey) {
		t.Errorf("a remote with a password printed %q", got)
	}
}
