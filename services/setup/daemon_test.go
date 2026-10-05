package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Every hint follows what setup left on the host: the service, the provider to start by hand, or no setup at all.
func TestLocalDaemonNamesTheHostsOwnSetup(t *testing.T) {
	cases := []struct {
		name, os string
		files    []string
		provider string
		want     Daemon
	}{
		{"linux service", "linux", []string{systemdUnit}, "", Daemon{"is it running? systemctl status shard", "sudo journalctl -u shard"}},
		{"mac service", "darwin", []string{launchdPlist}, "", Daemon{"is it running? launchctl print system/shard.daemon", "/var/log/shard/daemon.log"}},
		{"linux by hand", "linux", nil, Runc, Daemon{"is it running? sudo shard daemon --provider runc", foregroundLog}},
		{"mac by hand", "darwin", nil, VZ, Daemon{"is it running? shard daemon --provider vz", foregroundLog}},
		{"a Mac plist on linux is no service", "linux", []string{launchdPlist}, GVisor, Daemon{"is it running? sudo shard daemon --provider gvisor", foregroundLog}},
		{"linux, no setup", "linux", nil, "", Daemon{"is it set up? shard setup", foregroundLog}},
		{"mac, no setup", "darwin", nil, "", Daemon{"is it set up? shard setup", foregroundLog}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := Host{Root: t.TempDir(), OS: c.os, Env: sudoUser}
			for _, f := range c.files {
				put(t, h, f, nil)
			}
			if c.provider != "" {
				data, err := json.Marshal(Manifest{Version: "v0.1.1", Provider: c.provider})
				if err != nil {
					t.Fatal(err)
				}
				put(t, h, ManifestPath, data)
			}

			got, err := LocalDaemon(h)
			if err != nil {
				t.Fatalf("LocalDaemon: %v", err)
			}
			if got != c.want {
				t.Errorf("LocalDaemon = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Root itself runs a command bare, so neither hint tells it to use sudo. (SHARD-727)
func TestLocalDaemonHintsRootWithoutSudo(t *testing.T) {
	h := Host{Root: t.TempDir(), OS: "linux", Env: func(string) string { return "" }}
	put(t, h, systemdUnit, nil)
	got, err := LocalDaemon(h)
	if err != nil {
		t.Fatalf("LocalDaemon: %v", err)
	}
	if want := (Daemon{"is it running? systemctl status shard", "journalctl -u shard"}); got != want {
		t.Errorf("service: LocalDaemon = %+v, want %+v", got, want)
	}

	h.Root = t.TempDir()
	data, err := json.Marshal(Manifest{Version: "v0.1.1", Provider: GVisor})
	if err != nil {
		t.Fatal(err)
	}
	put(t, h, ManifestPath, data)
	got, err = LocalDaemon(h)
	if err != nil {
		t.Fatalf("LocalDaemon: %v", err)
	}
	if want := (Daemon{"is it running? shard daemon --provider gvisor", foregroundLog}); got != want {
		t.Errorf("by hand: LocalDaemon = %+v, want %+v", got, want)
	}
}

// sudoUser is the environment of a person who ran a command with sudo.
func sudoUser(name string) string {
	if name == "SUDO_USER" {
		return "u"
	}

	return ""
}

// A manifest setup cannot read is the error, never a guess at the daemon.
func TestLocalDaemonRefusesAManifestItCannotRead(t *testing.T) {
	h := Host{Root: t.TempDir(), OS: "linux", Env: sudoUser}
	put(t, h, ManifestPath, []byte("{"))

	if _, err := LocalDaemon(h); err == nil {
		t.Error("LocalDaemon read a broken manifest")
	}
}

func put(t *testing.T, h Host, path string, data []byte) {
	t.Helper()
	full := filepath.Join(h.Root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
