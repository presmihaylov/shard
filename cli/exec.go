package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// ExitError carries an exit code out to the process. A command that ran carries no message with it,
// because it already wrote whatever it had to write, and main prints nothing for that one.
type ExitError struct {
	Code int
	// Message is what shard has to say about a command that never ran at all.
	Message string
}

func (e *ExitError) Error() string {
	if e.Message != "" {
		return e.Message
	}

	return fmt.Sprintf("the command exited with code %d", e.Code)
}

// execOptions is one parsed shard exec invocation.
type execOptions struct {
	id   string
	argv []string

	env     []string
	workDir string
	user    string

	interactive bool
	tty         bool
}

// exec hands one command to the daemon and wears its exit code. Ctrl-C ends that command and nothing
// else: the daemon sees the connection go and cancels the exec, and only stop ends a sandbox.
func (a App) exec(ctx context.Context, args []string) error {
	opts, err := parseExec(args)
	if err != nil {
		return err
	}

	if opts.tty && !pty.IsTerminal(a.stdin()) {
		return errors.New("-t needs a terminal on stdin, and this one is not one")
	}

	req := sandbox.ExecRequest{
		Command: opts.argv,
		Env:     opts.env,
		WorkDir: opts.workDir,
		User:    opts.user,
		TTY:     opts.tty,
	}

	streams := client.ExecStreams{Stdout: a.Out, Stderr: a.Err, Warn: a.warn}
	if opts.interactive {
		streams.Stdin = a.stdin()
	}

	status, err := a.runExec(ctx, opts, req, streams)
	if err != nil {
		return shellCode(err)
	}

	if status.Code != 0 {
		return &ExitError{Code: status.Code}
	}

	return nil
}

func (a App) runExec(ctx context.Context, opts execOptions, req sandbox.ExecRequest, streams client.ExecStreams) (models.ExitStatus, error) {
	if !opts.tty {
		return a.client().Exec(ctx, opts.id, req, streams)
	}

	return a.execOnTerminal(ctx, opts, req, streams)
}

// execOnTerminal puts this terminal into raw mode, so a keystroke reaches the guest untouched. The
// guest's own terminal is the daemon's. The restore runs on every path out of here, a panic included.
func (a App) execOnTerminal(ctx context.Context, opts execOptions, req sandbox.ExecRequest, streams client.ExecStreams) (status models.ExitStatus, err error) {
	terminal := a.stdin()

	size, err := pty.SizeOf(terminal)
	if err != nil {
		return models.ExitStatus{}, err
	}
	req.Size = sandbox.TerminalSize{Rows: size.Rows, Cols: size.Cols}

	restore, err := pty.MakeRaw(terminal)
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, restore()) }()

	forwarder := forwardResize(ctx, a, opts.id, terminal)
	defer forwarder.stop()
	streams.Started = forwarder.named

	return a.client().Exec(ctx, opts.id, req, streams)
}

// resizes keeps the guest's window the size of this one. A SIGWINCH reaches the exec only once the
// daemon has named it, so one that arrives before that is applied as soon as the name does.
type resizes struct {
	app      App
	ref      string
	terminal *os.File

	changed chan os.Signal
	execIDs chan string
	done    chan struct{}
	exited  chan struct{}
}

func forwardResize(ctx context.Context, app App, ref string, terminal *os.File) *resizes {
	r := &resizes{
		app:      app,
		ref:      ref,
		terminal: terminal,
		changed:  make(chan os.Signal, 1),
		execIDs:  make(chan string, 1),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	signal.Notify(r.changed, syscall.SIGWINCH)

	go r.run(ctx)

	return r
}

// named is what the client calls with the id the daemon gave this exec.
func (r *resizes) named(execID string) { r.execIDs <- execID }

func (r *resizes) run(ctx context.Context) {
	defer close(r.exited)

	var execID string
	pending := false

	for {
		select {
		case <-r.done:
			return
		case execID = <-r.execIDs:
			if pending {
				r.resize(ctx, execID)
				pending = false
			}
		case <-r.changed:
			if execID == "" {
				pending = true

				continue
			}

			r.resize(ctx, execID)
		}
	}
}

func (r *resizes) resize(ctx context.Context, execID string) {
	size, err := pty.SizeOf(r.terminal)
	if err != nil {
		r.app.warn(fmt.Sprintf("read the terminal size: %v", err))

		return
	}

	if err := r.app.client().ResizeExec(ctx, r.ref, execID, sandbox.TerminalSize{Rows: size.Rows, Cols: size.Cols}); err != nil {
		r.app.warn(fmt.Sprintf("resize the command's terminal: %v", err))
	}
}

func (r *resizes) stop() {
	signal.Stop(r.changed)
	close(r.done)
	<-r.exited
}

// shellCode answers a command that never ran the way a shell does, because runsc reports every one
// of those as its own 128, which nothing outside runsc means anything by.
func shellCode(err error) error {
	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) {
		return err
	}

	return &ExitError{Code: notStarted.Code, Message: notStarted.Error()}
}

// parseExec splits the flags, the id and the argv. Go's flag stops at the first non-flag argument,
// so the flags precede the id, and everything after the literal -- is the command.
func parseExec(args []string) (execOptions, error) {
	var opts execOptions

	flags := flag.NewFlagSet("shard exec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&opts.interactive, "i", false, "keep stdin open on the command")
	flags.BoolVar(&opts.tty, "t", false, "run the command on a terminal")
	flags.Var((*envList)(&opts.env), "env", "an environment variable as KEY=VALUE, repeatable")
	flags.StringVar(&opts.workDir, "workdir", "", "the directory the command starts in")
	flags.StringVar(&opts.user, "user", "", "the user the command runs as")

	head, argv, separated := splitAtSeparator(args)

	if err := flags.Parse(expandBundles(head)); err != nil {
		return execOptions{}, fmt.Errorf("parse the exec flags: %w", err)
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return execOptions{}, errors.New("exec takes one sandbox id, got none")
	}
	if len(rest) > 1 {
		return execOptions{}, fmt.Errorf("unexpected argument %q: the flags go before the id and the command after --", rest[1])
	}

	if !separated {
		return execOptions{}, errors.New("exec takes the command to run after --, and there was no --")
	}
	if len(argv) == 0 {
		return execOptions{}, errors.New("-- takes the command to run, and nothing followed it")
	}

	// A terminal nothing can type on is a hang, and the guest would wait on a keyboard that never answers.
	if opts.tty && !opts.interactive {
		return execOptions{}, errors.New("-t needs -i: a terminal with no input on it is a command nobody can answer")
	}

	opts.id, opts.argv = rest[0], argv

	return opts, nil
}

// splitAtSeparator cuts at the first --, so a flag in the command is never read as one of shard's.
func splitAtSeparator(args []string) (head, tail []string, found bool) {
	at := slices.Index(args, "--")
	if at < 0 {
		return args, nil, false
	}

	return args[:at], args[at+1:], true
}

// expandBundles spells -it as two flags, because Go's flag package reads it as one named "it".
func expandBundles(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-it" || arg == "-ti" {
			out = append(out, "-i", "-t")

			continue
		}

		out = append(out, arg)
	}

	return out
}
