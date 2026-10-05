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
	savedLocal
	savedExit
)

// saved is the §14 menu for the connection saved at path, which takes the first screen's place; why is localOption's.
func (s *Setup) saved(ctx context.Context, path string, saved client.Config, why []string) error {
	if err := s.UI.Print("Saved connection: "+client.Redacted(saved.Remote), ""); err != nil {
		return err
	}
	choice, err := s.UI.Select(ctx, AskSaved, "What would you like to do?", []term.Option{
		savedCheck:   {Name: "check", Label: "Check this connection", Default: true},
		savedReplace: {Name: "replace", Label: "Replace this connection"},
		savedRemove:  {Name: "remove", Label: "Remove this connection"},
		savedLocal:   localOption(why),
		savedExit:    {Name: "exit", Label: "Exit"},
	})
	if err != nil {
		return err
	}

	switch choice {
	case savedCheck:
		return s.check(ctx, path, saved)
	case savedReplace:
		return s.connect(ctx, path, saved)
	case savedRemove:
		lines, err := s.forget(ctx, path)
		if err != nil {
			return err
		}

		return s.UI.Print(lines...)
	case savedLocal:
		return s.runLocal(ctx)
	}

	return nil
}

// check verifies the saved connection as it is; one edited after a failure is a replacement, saved only on the user's word.
func (s *Setup) check(ctx context.Context, path string, saved client.Config) error {
	conn, caps, err := s.verify(ctx, saved, saved)
	if err != nil {
		return err
	}
	if conn != saved {
		return s.offerSave(ctx, path, saved, conn, caps)
	}

	return s.UI.Print(connectedLines(caps)...)
}

// forget removes the saved connection and says what normal commands use now: the local daemon, or nothing set up yet.
func (s *Setup) forget(ctx context.Context, path string) ([]string, error) {
	_, installed, err := Detect(ctx, s.Host)
	if err != nil {
		return nil, err
	}
	if err := client.RemoveConfig(path); err != nil {
		return nil, err
	}
	lines := []string{"✓ Connection removed", ""}
	if note := remoteEnvNote(s.Host.Env, ""); note != nil {
		return append(lines, note...), nil
	}
	if !installed {
		return append(lines, "From now on, shard commands use this machine, which is not set up to run sandboxes.", "Run shard setup again to set it up."), nil
	}

	return append(lines, "From now on, shard commands use the local daemon."), nil
}

// handover is the §14 answer about a saved remote: the line the review adds, and finish, which acts on it once a local job succeeds and says what it did.
type handover struct {
	review string
	finish func(context.Context) ([]string, error)
}

// switchToLocal is §14: it asks now, and finish drops the saved connection only once local setup succeeds.
func (s *Setup) switchToLocal(ctx context.Context) (handover, error) {
	path, err := client.ConfigPath(s.Host.Env)
	if err != nil {
		return handover{}, err
	}
	saved, err := client.LoadConfig(path)
	if err != nil {
		return handover{}, err
	}
	if saved.Remote == "" {
		return handover{finish: func(context.Context) ([]string, error) { return remoteEnvNote(s.Host.Env, ""), nil }}, nil
	}

	current := []string{"Normal shard commands currently use the remote server " + client.Redacted(saved.Remote) + ", saved in " + path + ".", ""}
	if env := strings.TrimSpace(s.Host.Env(client.RemoteEnv)); env != "" {
		current = []string{
			"Normal shard commands currently use the remote server " + client.Redacted(env) + ", set in " + client.RemoteEnv + ".",
			"A connection to " + client.Redacted(saved.Remote) + " is also saved in " + path + ".", "",
		}
	}
	if err := s.UI.Print(current...); err != nil {
		return handover{}, err
	}
	remove, err := s.UI.Confirm(ctx, AskSwitch, "Remove the saved connection after local setup succeeds?", true)
	if err != nil {
		return handover{}, err
	}
	if !remove {
		return handover{finish: func(context.Context) ([]string, error) {
			lines := []string{"The saved connection remains, so normal shard commands still use the remote server.", "Run shard setup again to remove it."}
			return append(lines, remoteEnvNote(s.Host.Env, saved.Remote)...), nil
		}}, nil
	}

	return handover{
		review: "  Remove the saved connection to " + client.Redacted(saved.Remote) + ".",
		finish: func(ctx context.Context) ([]string, error) { return s.forget(ctx, path) },
	}, nil
}

// remoteEnvNote is the §14 reminder that SHARD_REMOTE beats what an unset brings back: the saved connection, else the local daemon.
func remoteEnvNote(env func(string) string, saved string) []string {
	remote := strings.TrimSpace(env(client.RemoteEnv))
	if remote == "" {
		return nil
	}
	if saved != "" {
		return []string{client.RemoteEnv + " is still set to " + client.Redacted(remote) + ", and it overrides the saved connection.", "Unset it to use the saved connection to " + client.Redacted(saved) + "."}
	}

	return []string{client.RemoteEnv + " is still set to " + client.Redacted(remote) + ", and it overrides the local default.", "Unset it to use the local daemon."}
}
