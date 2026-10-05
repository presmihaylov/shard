package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// foregroundLog is where a daemon started by hand writes its log: its own stderr.
const foregroundLog = "the terminal that runs shard daemon"

// Daemon is how this host runs the daemon of the default root, as setup left it.
type Daemon struct {
	// Hint is the question and the command that answers it, for a socket nothing answers on.
	Hint string
	// Log is where that daemon writes the cause of a failure.
	Log string
}

// LocalDaemon reads the host without changing it: the service it runs, else the provider setup left to start by hand, else no setup at all.
func LocalDaemon(h Host) (Daemon, error) {
	service, running := systemdUnit, Daemon{Hint: "is it running? systemctl status shard", Log: sudoFor(h) + "journalctl -u " + serviceName}
	if h.OS == "darwin" {
		service, running = launchdPlist, Daemon{Hint: "is it running? launchctl print " + launchdLabel, Log: macLogDir + "/daemon.log"}
	}
	_, err := os.Lstat(filepath.Join(h.Root, service))
	if err == nil {
		return running, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Daemon{}, fmt.Errorf("check %s: %w", service, err)
	}

	m, ok, err := LoadManifest(h)
	if err != nil {
		return Daemon{}, err
	}
	if !ok {
		return Daemon{Hint: "is it set up? shard setup", Log: foregroundLog}, nil
	}

	return Daemon{Hint: "is it running? " + socketSudo(h) + "shard daemon --provider " + m.Provider, Log: foregroundLog}, nil
}
