package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/pkg/store"
)

// ConfigHomeEnv moves the user's configuration directory, as the XDG base directory spec defines it.
const ConfigHomeEnv = "XDG_CONFIG_HOME"

// Config is the saved connection that shard setup writes; it holds the key as plain text, so only the user can read the file.
type Config struct {
	Remote string `json:"remote"`
	APIKey string `json:"api_key"`
}

// Format prints the remote alone, its password hidden, so a config in an error or a log line never shows its key.
func (c Config) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "saved connection to %s", Redacted(c.Remote))
}

// ConfigPath is $XDG_CONFIG_HOME/shard/config.json, or ~/.config/shard/config.json; the XDG spec ignores a relative XDG_CONFIG_HOME.
func ConfigPath(getenv func(string) string) (string, error) {
	if dir := getenv(ConfigHomeEnv); filepath.IsAbs(dir) {
		return filepath.Join(dir, "shard", "config.json"), nil
	}

	home := getenv("HOME")
	if home == "" {
		// A service manager may start a verb with no HOME, and the account database still names the home directory.
		current, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("find the shard configuration: HOME is not set and %w", err)
		}
		home = current.HomeDir
	}
	if home == "" {
		return "", errors.New("find the shard configuration: HOME is not set and the user has no home directory")
	}

	return filepath.Join(home, ".config", "shard", "config.json"), nil
}

// LoadConfig reads the saved connection at path. No file is the zero Config, since a host with no saved connection is the common case.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read the shard configuration %s: %w", path, err)
	}

	var c Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse the shard configuration %s: %w", path, err)
	}
	if err := c.check(); err != nil {
		return Config{}, fmt.Errorf("the shard configuration %s %w", path, err)
	}

	return c, nil
}

// SaveConfig replaces the file at path with c whole, by a rename, in a directory only the user can open.
func SaveConfig(path string, c Config) error {
	if c.Remote == "" || strings.TrimSpace(c.APIKey) == "" {
		return errors.New("a saved connection needs a remote and an API key")
	}
	if err := c.check(); err != nil {
		return fmt.Errorf("the connection %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ") //nolint:gosec // the file exists to hold the key, for its user alone
	if err != nil {
		return fmt.Errorf("encode the shard configuration: %w", err)
	}

	dir := filepath.Dir(path)
	if err := store.MkdirAllDurable(dir, 0o700); err != nil {
		return fmt.Errorf("create the shard configuration directory: %w", err)
	}
	// MkdirAll leaves the mode of a directory that was already there.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory needs its search bit, and only its user has it
		return fmt.Errorf("restrict %s to its user: %w", dir, err)
	}

	if err := store.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the shard configuration: %w", err)
	}

	return nil
}

// RemoveConfig deletes the saved connection; a file that is already gone is not an error.
func RemoveConfig(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove the shard configuration %s: %w", path, err)
	}

	if err := store.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("remove the shard configuration %s: %w", path, err)
	}

	return nil
}

// check refuses a remote the client cannot dial and a key no header can carry; an error never quotes the key.
func (c Config) check() error {
	if c.Remote != "" {
		if _, err := parseRemote(c.Remote); err != nil {
			return fmt.Errorf("holds a remote that %w", err)
		}
	}
	if err := checkToken(c.APIKey); err != nil {
		return fmt.Errorf("holds an API key that %w", err)
	}

	return nil
}
