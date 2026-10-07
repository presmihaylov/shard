package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
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
	// Message is what shard has to say about a command that never ran at all, or one whose output was cut.
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

// exec hands one command to the daemon and wears its exit code. Ctrl-C drops the connection and nothing
// else: the command runs on, to re-attach by its exec id, and only stop ends a sandbox.
func (a App) exec(ctx context.Context, args []string) error {
	opts, err := parseExec(args)
	if err != nil {
		return err
	}

	if opts.tty && !pty.IsTerminal(a.stdin()) {
		return errors.New("-t needs a terminal on stdin; run it in a terminal, or drop -t")
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

	return exitOf(a.runExec(ctx, opts, req, streams))
}

// exitOf is the verb's answer to a command's end: its exit code, or why it never ran or lost its output.
func exitOf(status models.ExitStatus, err error) error {
	// A gap in the output fails the verb even when the command passed, so a caller never takes a cut stream as whole.
	var lost *client.LostOutputError
	if errors.As(err, &lost) {
		return &ExitError{Code: max(lost.Exit.Code, 1), Message: lost.Error()}
	}
	if err != nil {
		return shellCode(err)
	}

	if status.Code != 0 {
		return &ExitError{Code: status.Code}
	}

	return nil
}

func (a App) runExec(ctx context.Context, opts execOptions, req sandbox.ExecRequest, streams client.ExecStreams) (models.ExitStatus, error) {
	c, err := a.client()
	if err != nil {
		return models.ExitStatus{}, err
	}

	if !opts.tty {
		return c.Exec(ctx, opts.id, req, streams)
	}

	return a.execOnTerminal(ctx, c, opts.id, req, streams)
}

// execOnTerminal puts this terminal into raw mode, so a keystroke reaches the guest untouched. The
// guest's own terminal is the daemon's. The restore runs on every path out of here, a panic included.
func (a App) execOnTerminal(ctx context.Context, c *client.Client, ref string, req sandbox.ExecRequest, streams client.ExecStreams) (status models.ExitStatus, err error) {
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

	forwarder := forwardResize(ctx, a, c, ref, terminal)
	defer forwarder.stop()
	streams.Started = forwarder.named

	return c.Exec(ctx, ref, req, streams)
}

// resizes keeps the guest's window the size of this one. A SIGWINCH reaches the exec only once the
// daemon has named it, so one that arrives before that is applied as soon as the name does.
type resizes struct {
	app      App
	client   *client.Client
	ref      string
	terminal *os.File

	changed chan os.Signal
	execIDs chan string
	done    chan struct{}
	exited  chan struct{}
	// cancel ends a resize in flight, which would otherwise hold stop, and the raw terminal, for the client's whole timeout.
	cancel context.CancelFunc
}

func forwardResize(ctx context.Context, app App, c *client.Client, ref string, terminal *os.File) *resizes {
	ctx, cancel := context.WithCancel(ctx)
	r := &resizes{
		app:      app,
		client:   c,
		ref:      ref,
		terminal: terminal,
		changed:  make(chan os.Signal, 1),
		execIDs:  make(chan string, 1),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
		cancel:   cancel,
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

	err = r.client.ResizeExec(ctx, r.ref, execID, sandbox.TerminalSize{Rows: size.Rows, Cols: size.Cols})
	// A resize the exit cancelled is moot, since the command is over, and a warning would land on the raw terminal.
	if err != nil && ctx.Err() == nil {
		r.app.warn(fmt.Sprintf("resize the command's terminal: %v", err))
	}
}

func (r *resizes) stop() {
	signal.Stop(r.changed)
	r.cancel()
	close(r.done)
	<-r.exited
}

// shellCode answers a command that never ran the way a shell does, because runsc reports every one
// of those as its own 128, which nothing outside runsc means anything by.
func shellCode(err error) error {
	code, message, ok := notStarted(err)
	if !ok {
		return err
	}

	return &ExitError{Code: code, Message: message}
}

// notStarted reads a command that never ran from either side of the socket: a refused run or an attach that ended so.
func notStarted(err error) (int, string, bool) {
	var refused *client.APIError
	if errors.As(err, &refused) && refused.Code == models.CodeCommandNotStarted {
		return refused.ExitCode, refused.Message, true
	}

	var never *models.CommandNotStartedError
	if !errors.As(err, &never) {
		return 0, "", false
	}

	return never.Code, never.Error(), true
}

// Go stops at the sandbox reference, so guest arguments never reach the flag parser.
func parseExec(args []string) (execOptions, error) {
	var opts execOptions

	flags := newFlags("exec")
	flags.BoolVar(&opts.interactive, "i", false, "")
	flags.BoolVar(&opts.tty, "t", false, "")
	flags.BoolVar(&opts.interactive, "interactive", false, "")
	flags.BoolVar(&opts.tty, "tty", false, "")
	flags.Var((*envList)(&opts.env), "env", "")
	flags.StringVar(&opts.workDir, "workdir", "", "")
	flags.StringVar(&opts.user, "user", "", "")
	var refused error
	runFlags(flags, &refused)

	if err := parseVerb(flags, expandBundles(args, flags)); err != nil {
		return execOptions{}, err
	}
	if refused != nil {
		return execOptions{}, refused
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return execOptions{}, errors.New("exec takes one sandbox id or name, got none")
	}
	argv := rest[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return execOptions{}, errors.New("exec takes a command after the sandbox id or name")
	}

	// A terminal nothing can type on is a hang, and the guest would wait on a keyboard that never answers.
	if opts.tty && !opts.interactive {
		return execOptions{}, errors.New("-t needs -i; pass -it")
	}

	opts.id, opts.argv = rest[0], argv

	return opts, nil
}

// expandBundles leaves flag values and all arguments after the sandbox reference untouched.
func expandBundles(args []string, flags *flag.FlagSet) []string {
	out := make([]string, 0, len(args))
	for at := 0; at < len(args); at++ {
		arg := args[at]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			return append(out, args[at:]...)
		}
		if arg == "-it" || arg == "-ti" {
			out = append(out, "-i", "-t")

			continue
		}

		out = append(out, arg)
		name, _, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := flags.Lookup(name)
		if f == nil || inline {
			continue
		}
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if at+1 < len(args) {
			at++
			out = append(out, args[at])
		}
	}

	return out
}
