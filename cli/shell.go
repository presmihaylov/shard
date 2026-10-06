package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// shellScript picks the shell in the sandbox, and no login shell, whose /etc/profile resets the image's PATH.
const shellScript = "command -v bash >/dev/null 2>&1 && exec bash -i; exec sh -i"

// shellOptions is one parsed shard shell invocation.
type shellOptions struct {
	id      string
	workDir string
	user    string
}

// shell opens an interactive shell on this terminal over the exec routes exec -it takes, so a remote needs no new scope.
func (a App) shell(ctx context.Context, args []string) error {
	opts, err := parseShell(args)
	if err != nil {
		return err
	}

	if !pty.IsTerminal(a.stdin()) {
		return errors.New("shell needs a terminal on stdin; to run a command without one, use shard exec")
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	req := sandbox.ExecRequest{
		Command: []string{"sh", "-c", shellScript},
		WorkDir: opts.workDir,
		User:    opts.user,
		TTY:     true,
	}
	streams := client.ExecStreams{Stdin: a.stdin(), Stdout: a.Out, Stderr: a.Err, Warn: a.warn}

	status, err := a.execOnTerminal(ctx, c, opts.id, req, streams)
	if code, message, ok := notStarted(err); ok && code == models.CommandNotFoundExitCode {
		return a.noShell(ctx, c, opts, message)
	}

	return exitOf(status, err)
}

// noShell asks again from /, because a missing --workdir refuses the start with the same 127 a missing sh does.
func (a App) noShell(ctx context.Context, c *client.Client, opts shellOptions, refused string) error {
	probe := sandbox.ExecRequest{Command: []string{"sh", "-c", ":"}, WorkDir: "/", User: opts.user}
	_, err := c.Exec(ctx, opts.id, probe, client.ExecStreams{Stdout: io.Discard, Stderr: io.Discard, Warn: a.warn})

	code, _, ok := notStarted(err)
	if ok && code == models.CommandNotFoundExitCode {
		return &ExitError{Code: code, Message: fmt.Sprintf("shard found no shell in sandbox %s: it has neither bash nor sh", opts.id)}
	}
	if err != nil && !ok {
		refused = fmt.Sprintf("%s; and the check for a shell failed: %v", refused, err)
	}

	return &ExitError{Code: models.CommandNotFoundExitCode, Message: refused}
}

func parseShell(args []string) (shellOptions, error) {
	var opts shellOptions

	flags := newFlags("shell")
	flags.StringVar(&opts.workDir, "workdir", "", "")
	flags.StringVar(&opts.user, "user", "", "")

	if err := parseVerb(flags, args); err != nil {
		return shellOptions{}, err
	}

	rest := flags.Args()
	if len(rest) != 1 {
		return shellOptions{}, fmt.Errorf("shell takes one sandbox id or name, got %s; to run a command, use shard exec", gotArgs(rest))
	}

	opts.id = rest[0]

	return opts, nil
}
