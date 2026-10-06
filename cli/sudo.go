package cli

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/presmihaylov/shard/services/client"
)

// forUser words the shard commands err names for the user who ran this process, so each works as printed.
func (a App) forUser(err error) error {
	if err == nil {
		return nil
	}
	sudo, savedErr := a.sudoHints()
	if savedErr != nil {
		return errors.Join(err, savedErr)
	}
	if !sudo {
		return err
	}
	// main prints an ExitError's own message, so sudo goes in it and the exit code stays.
	if exit, ok := errors.AsType[*ExitError](err); ok {
		exit.Message = withSudo(exit.Message)

		return err
	}

	return sudoError{err: err}
}

// hinted is forUser for a line a verb prints itself.
func (a App) hinted(text string) (string, error) {
	sudo, err := a.sudoHints()
	if err != nil || !sudo {
		return text, err
	}

	return withSudo(text), nil
}

// sudoHints says a hint gains sudo: this process runs under sudo, on this host's socket. Under sudo a remote command loses the user's saved connection and key.
func (a App) sudoHints() (bool, error) {
	if !a.sudoed() {
		return false, nil
	}
	saved, err := a.saved()
	if err != nil {
		return false, err
	}

	return cmp.Or(a.Remote, saved.Remote) == "", nil
}

func (a App) sudoed() bool {
	if a.asSudo != nil {
		return a.asSudo()
	}

	return underSudo(os.Geteuid(), os.Getenv)
}

// sudoOwner is the user sudo ran this process for, who takes what a copy writes on the host; nil when there is no sudo.
func (a App) sudoOwner() (*client.Owner, error) {
	if !a.sudoed() {
		return nil, nil
	}
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil {
		return nil, fmt.Errorf("read SUDO_UID: %w", err)
	}
	gid, err := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err != nil {
		return nil, fmt.Errorf("read SUDO_GID: %w", err)
	}

	return &client.Owner{UID: uid, GID: gid}, nil
}

// underSudo says root runs this process for a user who started it with sudo; root itself runs a command bare.
func underSudo(euid int, env func(string) string) bool {
	return euid == 0 && env("SUDO_USER") != ""
}

// sudoError is err with sudo before each shard command it names; errors.As and errors.Is still reach err.
type sudoError struct{ err error }

func (s sudoError) Error() string { return withSudo(s.err.Error()) }

func (s sudoError) Unwrap() error { return s.err }

// commandHint is a shard verb after the words a hint starts a command with, so prose such as "a shard daemon" stays as it is.
func commandHint() *regexp.Regexp {
	verbs := append(names(commands()), "help")

	return regexp.MustCompile(`(?:\b(?:with|[Rr]un)|[:;|]) (?:sudo )?shard (?:` + strings.Join(verbs, "|") + `)\b`)
}

// withSudo puts sudo before each shard command text names that has none.
func withSudo(text string) string {
	return commandHint().ReplaceAllStringFunc(text, func(hint string) string {
		if strings.Contains(hint, "sudo ") {
			return hint
		}

		return strings.Replace(hint, "shard ", "sudo shard ", 1)
	})
}
