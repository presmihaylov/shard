package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// runFailedExitCode is docker run's code for its own failure, so a script tells shard failing from an app that exited 1.
const runFailedExitCode = 125

// runOptions is one parsed shard run: the sandbox to create with its app, and whether to wait on the app.
type runOptions struct {
	req    sandbox.CreateRequest
	detach bool
}

// launch creates a sandbox whose app is the command, then waits on the app and exits with its last code; -d prints the id instead.
func (a App) launch(ctx context.Context, args []string) error {
	err := a.runApp(ctx, args)

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

func (a App) runApp(ctx context.Context, args []string) error {
	opts, err := parseRun(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := a.createAndWait(ctx, c, opts.req)
	if err != nil {
		return err
	}
	if opts.detach {
		return a.print(sb.ID)
	}

	return a.attachApp(ctx, c, sb.ID)
}

// attachedApp is how the attach ended: the app's last exit, or why the attach failed.
type attachedApp struct {
	exit models.AppExit
	err  error
}

// attachApp prints the app's output until its policy ends; interrupts stop, then kill, then leave, and the sandbox runs on.
func (a App) attachApp(ctx context.Context, c *client.Client, id string) error {
	interrupts := a.Interrupts.take()
	// An interrupt that raced the take cancelled the work, and the operator meant it for the app.
	if ctx.Err() != nil {
		return fmt.Errorf("sandbox %s runs on; shard logs %s shows its app: %w", id, id, ctx.Err())
	}

	attached := make(chan attachedApp, 1)
	go func() {
		exit, err := c.AttachApp(ctx, id, a.Out)
		attached <- attachedApp{exit: exit, err: err}
	}()

	stopped := make(chan error, 2)
	asked := 0
	for {
		select {
		case result := <-attached:
			if result.err != nil {
				return result.err
			}
			if result.exit.Code != 0 {
				return &ExitError{Code: result.exit.Code}
			}

			return nil
		case err := <-stopped:
			if err != nil {
				return err
			}
		case <-interrupts:
			asked++
			if asked > 2 {
				return &ExitError{Code: InterruptedExitCode, Message: fmt.Sprintf("left the run; sandbox %s stays running", id)}
			}

			force := asked == 2
			a.note(stopNote(force))
			go func() { stopped <- stopApp(ctx, c, id, force) }()
		}
	}
}

func stopNote(force bool) string {
	if force {
		return "killing the app; Ctrl+C again to leave"
	}

	return "stopping the app; Ctrl+C again to kill it"
}

// stopApp asks the daemon to stop the app; one that already ended is what the stop asked for, and the attach brings its exit.
func stopApp(ctx context.Context, c *client.Client, id string, force bool) error {
	err := c.StopApp(ctx, id, force)
	var answer *client.APIError
	if errors.As(err, &answer) && answer.Code == models.CodeAppEnded {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stop the app of sandbox %s: %w", id, err)
	}

	return nil
}

// note tells the operator what shard does on their behalf, on stderr, so stdout stays the app's alone.
func (a App) note(message string) {
	if a.Err == nil {
		return
	}

	fmt.Fprintln(a.Err, "shard:", message)
}

// parseRun is create's flags, the restart policy and -d, then the image and the command, which run needs.
func parseRun(args []string) (runOptions, error) {
	var opts runOptions

	flags := newFlags("run")
	sandboxFlags(flags, &opts.req)
	var restart restartFlags
	flags.StringVar(&restart.policy, "restart", "", "")
	flags.IntVar(&restart.retries, "restart-retries", 0, "")
	flags.DurationVar(&restart.backoff, "restart-backoff", 0, "")
	flags.BoolVar(&opts.detach, "d", false, "")
	flags.BoolVar(&opts.detach, "detach", false, "")

	if err := parseVerb(flags, args); err != nil {
		return runOptions{}, err
	}

	var err error
	if opts.req.Restart, err = restart.request(); err != nil {
		return runOptions{}, err
	}
	if err := checkSandbox(flags, opts.req); err != nil {
		return runOptions{}, err
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return runOptions{}, errors.New("run takes one image reference, got none")
	}

	opts.req.Image, rest = rest[0], rest[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return runOptions{}, errors.New("run needs a command after the image")
	}
	opts.req.Command = rest

	return opts, nil
}
