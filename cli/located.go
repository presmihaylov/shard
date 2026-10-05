package cli

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/presmihaylov/shard/services/setup"
)

// logHasTheCause ends every daemon text whose cause it kept to its own log.
const logHasTheCause = "log has the cause"

// localDaemon is how this host runs the daemon of the default root, read from what setup left.
func (a App) localDaemon() (setup.Daemon, error) {
	host, err := setup.NewHost(a.Version)
	if err != nil {
		return setup.Daemon{}, err
	}

	return setup.LocalDaemon(host)
}

// located names where the log is, after an error from the default root that sends the reader to it.
func (a App) located(err error) error {
	if err == nil || a.Root != DefaultRoot || !strings.Contains(err.Error(), logHasTheCause) {
		return err
	}
	saved, savedErr := a.saved()
	if savedErr != nil {
		return errors.Join(err, savedErr)
	}
	if cmp.Or(a.Remote, saved.Remote) != "" {
		return err
	}
	daemon, daemonErr := a.localDaemon()
	if daemonErr != nil {
		return errors.Join(err, daemonErr)
	}

	return withLog(err, daemon.Log)
}

// withLog puts log after the text; main prints an ExitError's own message, so there the log goes in it and the exit code stays.
func withLog(err error, log string) error {
	var exit *ExitError
	if errors.As(err, &exit) && strings.Contains(exit.Message, logHasTheCause) {
		exit.Message += " (" + log + ")"

		return err
	}

	return fmt.Errorf("%w (%s)", err, log)
}
