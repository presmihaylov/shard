package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// ExitError carries a guest command's own exit code out to the process. It is not a failure of
// shard, so main prints nothing for it: the command already wrote whatever it had to write.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("the command exited with code %d", e.Code) }

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

// execRepo is the part of sandboxstate.Repository that exec drives.
type execRepo interface {
	Get(id string) (models.Sandbox, error)
}

// execProvider is the one verb exec drives.
type execProvider interface {
	Exec(ctx context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error)
}

// execDeps is what exec wires together. A test replaces the factory, because the real parts need root.
type execDeps struct {
	repo     execRepo
	provider execProvider

	// The terminal this shard process holds. A test replaces them: a pipe is not a terminal.
	stdin  *os.File
	stdout *os.File
	stderr *os.File
}

func (a App) exec(ctx context.Context, args []string) error {
	opts, err := parseExec(args)
	if err != nil {
		return err
	}

	build := a.newExecDeps
	if build == nil {
		build = defaultExecDeps
	}

	deps, err := build(a)
	if err != nil {
		return err
	}
	deps = deps.withHostStdio()

	if opts.tty && !pty.IsTerminal(deps.stdin) {
		return errors.New("-t needs a terminal on stdin, and this one is not one")
	}

	// The record answers for an id nobody ever created; the provider answers for its state.
	if _, err := deps.repo.Get(opts.id); err != nil {
		return err
	}

	spec := models.ExecSpec{
		Argv:    opts.argv,
		Env:     opts.env,
		WorkDir: opts.workDir,
		User:    opts.user,
		TTY:     opts.tty,
	}

	status, err := a.runExec(ctx, deps, opts, spec)
	if err != nil {
		return err
	}

	if status.Code != 0 {
		return &ExitError{Code: status.Code}
	}

	return nil
}

// runExec picks the stdio the guest process gets. Ctrl-C during either path ends that process and
// nothing else: the cancelled exec signals the one guest pid, and only stop ends a sandbox.
func (a App) runExec(ctx context.Context, deps execDeps, opts execOptions, spec models.ExecSpec) (models.ExitStatus, error) {
	if opts.tty {
		return a.execOnTerminal(ctx, deps, opts, spec)
	}

	if opts.interactive {
		spec.Stdin = deps.stdin
	}
	spec.Stdout, spec.Stderr = deps.stdout, deps.stderr

	return deps.provider.Exec(ctx, opts.id, spec)
}

// execOnTerminal gives the guest a pty replica and puts this terminal into raw mode, so a keystroke
// reaches the guest untouched. The restore runs on every path out of here, a panic included.
func (a App) execOnTerminal(ctx context.Context, deps execDeps, opts execOptions, spec models.ExecSpec) (status models.ExitStatus, err error) {
	pair, err := pty.Open()
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, pair.Close()) }()

	size, err := pty.SizeOf(deps.stdin)
	if err != nil {
		return models.ExitStatus{}, err
	}
	if err := pair.Resize(size); err != nil {
		return models.ExitStatus{}, err
	}

	restore, err := pty.MakeRaw(deps.stdin)
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, restore()) }()

	stop := forwardResize(pair, deps.stdin, a.warn)
	defer stop()

	// A terminal carries one stream, so all three fds are the same file.
	spec.Stdin, spec.Stdout, spec.Stderr = pair.Replica, pair.Replica, pair.Replica

	drained := pump(pair, deps, a.warn)

	status, err = deps.provider.Exec(ctx, opts.id, spec)

	// Our copy of the replica is what keeps the master readable, so the output drains only after it goes.
	if closeErr := pair.Replica.Close(); closeErr != nil {
		return status, errors.Join(err, fmt.Errorf("close the pseudo terminal replica: %w", closeErr))
	}
	<-drained

	return status, err
}

// pump moves bytes both ways and reports when the guest side has nothing left to say. The keyboard
// copier is left running: it blocks on a terminal shard does not own, and the process is about to end.
func pump(pair *pty.Pty, deps execDeps, warn func(string)) <-chan struct{} {
	go func() {
		if _, err := io.Copy(pair.Master, deps.stdin); err != nil {
			warn(fmt.Sprintf("the keyboard stopped reaching the command: %v", err))
		}
	}()

	drained := make(chan struct{})
	go func() {
		defer close(drained)

		// The master reports the replica's last close as an error, and that is the normal end of a session.
		if _, err := io.Copy(deps.stdout, pair.Master); err != nil && !errors.Is(err, syscall.EIO) {
			warn(fmt.Sprintf("the command's output stopped reaching the terminal: %v", err))
		}
	}()

	return drained
}

// forwardResize keeps the guest's window the size of this one. Writing the size to the master is
// what raises SIGWINCH inside the sandbox.
func forwardResize(pair *pty.Pty, terminal *os.File, warn func(string)) func() {
	changed := make(chan os.Signal, 1)
	signal.Notify(changed, syscall.SIGWINCH)

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-changed:
				size, err := pty.SizeOf(terminal)
				if err != nil {
					warn(fmt.Sprintf("read the terminal size: %v", err))

					continue
				}
				if err := pair.Resize(size); err != nil {
					warn(fmt.Sprintf("resize the command's terminal: %v", err))
				}
			}
		}
	}()

	return func() {
		signal.Stop(changed)
		close(done)
	}
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

// withHostStdio fills in the terminal this process holds, which is what the real command runs on.
func (d execDeps) withHostStdio() execDeps {
	if d.stdin == nil {
		d.stdin = os.Stdin
	}
	if d.stdout == nil {
		d.stdout = os.Stdout
	}
	if d.stderr == nil {
		d.stderr = os.Stderr
	}

	return d
}

// defaultExecDeps builds the real layers, which all refuse off Linux.
func defaultExecDeps(a App) (execDeps, error) {
	repo, err := sandboxstate.New(a.Root)
	if err != nil {
		return execDeps{}, err
	}

	// The mode is fixed on the runner, and it must match the one the sandbox was created with.
	runner, err := runsc.New(filepath.Join(a.Root, "runsc"), runsc.WithNetwork(runsc.NetworkSandbox))
	if err != nil {
		return execDeps{}, err
	}

	bundles, err := bundle.New(a.InitPath)
	if err != nil {
		return execDeps{}, err
	}

	provider, err := gvisor.New(runner, bundles, repo.Dir)
	if err != nil {
		return execDeps{}, err
	}

	return execDeps{repo: repo, provider: provider}, nil
}
