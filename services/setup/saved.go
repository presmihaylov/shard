package setup

import (
	"context"
	"strings"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
)

// The §14 choices for a saved connection, in the order the wizard shows them.
const (
	savedCheck = iota
	savedReplace
	savedRemove
	savedExit
)

var savedOptions = []term.Option{
	savedCheck:   {Name: "check", Label: "Check this connection", Default: true},
	savedReplace: {Name: "replace", Label: "Replace this connection"},
	savedRemove:  {Name: "remove", Label: "Remove this connection"},
	savedExit:    {Name: "exit", Label: "Exit"},
}

// saved is the §14 menu for the connection saved at path.
func (s *Setup) saved(ctx context.Context, path string, saved client.Config) error {
	if err := s.UI.Print("Saved connection: "+client.Redacted(saved.Remote), ""); err != nil {
		return err
	}
	choice, err := s.UI.Select(ctx, AskSaved, "What would you like to do?", savedOptions)
	if err != nil {
		return err
	}

	switch choice {
	case savedCheck:
		return s.check(ctx, path, saved)
	case savedReplace:
		return s.connect(ctx, path, saved)
	case savedRemove:
		return s.forget(path)
	}

	return nil
}

// check verifies the saved connection as it is; one edited after a failure is a replacement, saved only on the user's word.
func (s *Setup) check(ctx context.Context, path string, saved client.Config) error {
	conn, caps, err := s.verify(ctx, saved)
	if err != nil {
		return err
	}
	if conn != saved {
		return s.offerSave(ctx, path, saved, conn, caps)
	}

	return s.UI.Print(connectedLines(conn, caps)...)
}

// forget removes the saved connection and says what normal commands use now.
func (s *Setup) forget(path string) error {
	if err := client.RemoveConfig(path); err != nil {
		return err
	}
	lines := []string{"✓ Connection removed", ""}
	if note := remoteEnvNote(s.Host.Env); note != nil {
		return s.UI.Print(append(lines, note...)...)
	}

	return s.UI.Print(append(lines, "Shard commands now use the local daemon.")...)
}

// switchToLocal is §14: it asks now, and finish drops the saved connection only once local setup succeeds.
func (s *Setup) switchToLocal(ctx context.Context) (finish func(context.Context) error, err error) {
	path, err := client.ConfigPath(s.Host.Env)
	if err != nil {
		return nil, err
	}
	saved, err := client.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if saved.Remote == "" {
		return func(context.Context) error { return s.printNote(remoteEnvNote(s.Host.Env)) }, nil
	}

	if err := s.UI.Print("Normal Shard commands currently use the remote server "+client.Redacted(saved.Remote)+", saved in "+path+".", ""); err != nil {
		return nil, err
	}
	remove, err := s.UI.Confirm(ctx, AskSwitch, "Remove the saved connection after local setup succeeds?", true)
	if err != nil {
		return nil, err
	}
	if !remove {
		return func(context.Context) error {
			lines := []string{"", "The saved connection remains, so normal Shard commands still use the remote server.", "Run shard setup again to remove it."}
			return s.printNote(append(lines, remoteEnvNote(s.Host.Env)...))
		}, nil
	}

	return func(context.Context) error {
		if err := s.UI.Print(""); err != nil {
			return err
		}

		return s.forget(path)
	}, nil
}

// printNote prints lines when there are any, so a run with nothing to say prints nothing.
func (s *Setup) printNote(lines []string) error {
	if len(lines) == 0 {
		return nil
	}

	return s.UI.Print(lines...)
}

// remoteEnvNote is the §14 reminder that SHARD_REMOTE beats the local daemon, or nothing when it is unset.
func remoteEnvNote(env func(string) string) []string {
	remote := strings.TrimSpace(env(client.RemoteEnv))
	if remote == "" {
		return nil
	}

	return []string{client.RemoteEnv + " is still set to " + client.Redacted(remote) + ", and it overrides the local default.", "Unset it to use the local daemon."}
}
