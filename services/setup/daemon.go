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

// serviceHint names the fix, not a check: the repair of shard setup starts a service it installed and that stopped. (SHARD-735)
const serviceHint = "is the background service running? shard setup repairs it"

// Daemon is how this host runs the daemon of the default root, as setup left it.
type Daemon struct {
	// Hint is the question and the command that fixes it, for a socket nothing answers on.
	Hint string
	// Log is where that daemon writes the cause of a failure.
	Log string
}

// LocalDaemon reads the host without changing it: the service it runs, else the provider setup left to start by hand, else no setup at all.
func LocalDaemon(h Host) (Daemon, error) {
	service, check, log := systemdUnit, "systemctl status "+serviceName, sudoFor(h)+"journalctl -u "+serviceName
	if h.OS == "darwin" {
		service, check, log = launchdPlist, "launchctl print "+launchdLabel, macLogDir+"/daemon.log"
	}
	_, err := os.Lstat(filepath.Join(h.Root, service))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Daemon{}, fmt.Errorf("check %s: %w", service, err)
	}
	installed := err == nil

	m, ok, err := LoadManifest(h)
	if err != nil {
		return Daemon{}, err
	}
	if installed && ok && m.StartAtBoot {
		return Daemon{Hint: serviceHint, Log: log}, nil
	}
	// setup repairs only the service it installed, so one installed by hand keeps its own check.
	if installed {
		return Daemon{Hint: "is it running? " + check, Log: log}, nil
	}
	if !ok {
		return Daemon{Hint: "is it set up? shard setup", Log: foregroundLog}, nil
	}

	return Daemon{Hint: "is it running? " + socketSudo(h) + "shard daemon" + daemonArgs(m.Provider, m.StorageMiB), Log: foregroundLog}, nil
}
