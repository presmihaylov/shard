package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// runFailedExitCode is docker run's code for its own failure, so a script tells shard failing from a process that exited 1.
const runFailedExitCode = 125

// runOptions is one parsed shard run: the sandbox, the process to start in it, and whether to stay for its output.
type runOptions struct {
	id     string
	req    sandbox.RunRequest
	attach bool
}

// runProcess starts a named process in a running sandbox and prints its name; --attach stays for its output and exits with its code.
func (a App) runProcess(ctx context.Context, args []string) error {
	err := shellCode(a.startProcess(ctx, args))

	var exit *ExitError
	var help printExit
	if err == nil || errors.As(err, &exit) || errors.As(err, &help) {
		return err
	}
	if ctx.Err() != nil {
		return &ExitError{Code: InterruptedExitCode, Message: err.Error()}
	}

	return &ExitError{Code: runFailedExitCode, Message: err.Error()}
}

func (a App) startProcess(ctx context.Context, args []string) error {
	opts, err := parseRun(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	p, err := c.Run(ctx, opts.id, opts.req)
	if err != nil {
		return err
	}
	if !opts.attach {
		return a.print(p.Name)
	}

	return a.attachProcess(ctx, c, opts.id, p.Name)
}

// attachProcess prints the output until the policy ends the process, and exits with its code; an interrupt leaves it running.
func (a App) attachProcess(ctx context.Context, c *client.Client, id, name string) error {
	p, err := c.AttachProcess(ctx, id, name, a.Out)
	if err != nil && ctx.Err() != nil {
		a.note(fmt.Sprintf("detached; process %s runs on, and shard kill %s %s stops it", name, id, name))

		return nil
	}
	if err != nil {
		return err
	}

	return processExit(id, p)
}

// processExit is the code a shell gives the process's last exit, 128 and the signal for one a signal ended.
func processExit(id string, p models.Process) error {
	exit := p.Status.Exit
	if exit == nil {
		return fmt.Errorf("process %s of sandbox %s is %s and left no exit status", p.Name, id, p.Status.State)
	}

	code := exit.Code
	if exit.Signal != 0 {
		code = 128 + exit.Signal
	}
	if code == 0 {
		return nil
	}

	return &ExitError{Code: code}
}

// note tells the operator what shard does on their behalf, on stderr, so stdout stays the process's alone.
func (a App) note(message string) {
	if a.Err == nil {
		return
	}

	fmt.Fprintln(a.Err, "shard:", message)
}

// parseRun takes the flags on either side of the sandbox, and everything after the first argument past it as the command.
func parseRun(args []string) (runOptions, error) {
	var opts runOptions

	flags := newFlags("run")
	flags.StringVar(&opts.req.Name, "name", "", "")
	flags.Var((*envList)(&opts.req.Env), "e", "")
	flags.Var((*envList)(&opts.req.Env), "env", "")
	flags.StringVar(&opts.req.WorkDir, "w", "", "")
	flags.StringVar(&opts.req.WorkDir, "workdir", "", "")
	flags.StringVar(&opts.req.User, "u", "", "")
	flags.StringVar(&opts.req.User, "user", "", "")
	var restart restartFlags
	flags.StringVar(&restart.policy, "restart", "", "")
	flags.IntVar(&restart.retries, "restart-retries", 0, "")
	flags.DurationVar(&restart.backoff, "restart-backoff", 0, "")
	flags.BoolVar(&opts.attach, "attach", false, "")

	refs, command, err := parseAround(flags, args, 1)
	if err != nil {
		return runOptions{}, err
	}
	if len(refs) == 0 {
		return runOptions{}, errors.New("run takes one sandbox id or name, got none")
	}
	opts.id, opts.req.Command = refs[0], command
	if len(opts.req.Command) == 0 {
		return runOptions{}, errors.New("run takes a command after the sandbox id or name: shard " + helps["run"].usage[0])
	}

	if opts.req.Restart, err = restart.request(); err != nil {
		return runOptions{}, err
	}
	// An empty name would read as none, and the daemon would name the process after its command instead.
	if named(flags) && opts.req.Name == "" {
		return runOptions{}, errors.New("--name needs a process name; leave it out to name the process after its command")
	}

	return opts, nil
}
