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
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
)

// drainBudget is how long the command's last output may take to arrive once the command itself is gone.
const drainBudget = 2 * time.Second

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

func (a App) exec(ctx context.Context, args []string) error {
	opts, err := parseExec(args)
	if err != nil {
		return err
	}

	d := a.deps()

	repo, err := d.repo()
	if err != nil {
		return err
	}

	provider, err := d.provider()
	if err != nil {
		return err
	}

	if opts.tty && !pty.IsTerminal(d.stdin()) {
		return errors.New("-t needs a terminal on stdin, and this one is not one")
	}

	// Everything below this line names an id, so a name becomes one here and nowhere else.
	opts.id, err = repo.Resolve(opts.id)
	if err != nil {
		return err
	}

	// The record answers for an id nobody ever created; the provider answers for its state.
	sb, err := repo.Get(opts.id)
	if err != nil {
		return err
	}

	// A record that says stopped outranks the oom count the cgroup kept, and a paused one never has a cgroup.
	if sb.State == models.StateStopped {
		return refuseStopped(opts.id, sb.State)
	}

	// The provider holds nothing of a paused sandbox, and gone is the wrong word for one a resume brings back.
	if sb.State == models.StatePaused {
		return fmt.Errorf("sandbox %s is paused: resume it with shard resume %s", opts.id, opts.id)
	}

	if err := refuseUnlessAlive(ctx, provider, opts.id); err != nil {
		return err
	}

	spec := models.ExecSpec{
		Argv:    opts.argv,
		Env:     opts.env,
		WorkDir: opts.workDir,
		User:    opts.user,
		TTY:     opts.tty,
	}

	status, err := a.runExec(ctx, d, provider, opts, spec)
	if err != nil {
		return shellCode(err)
	}

	if status.Code != 0 {
		return &ExitError{Code: status.Code}
	}

	return nil
}

// refuseUnlessAlive asks the substrate, not the record, because a record saying running outlives a
// shard restart. A sandbox whose entrypoint exited is still alive and still takes an exec; a created
// one is alive too, and the provider refuses that one by name because only it knows what it holds.
func refuseUnlessAlive(ctx context.Context, provider models.Provider, id string) error {
	status, err := provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Alive() {
		return nil
	}

	// The exit file records a 137 for this, which is what a plain kill -9 records too, so the reason
	// is named here or an operator never learns it.
	if status.OOMKilled {
		return fmt.Errorf("sandbox %s ran out of memory and the host ended it: remove it with shard rm %s and create another with a larger --memory", id, id)
	}

	if !status.Exists {
		return fmt.Errorf("sandbox %s is gone from %s: remove it with shard rm %s and create another", id, provider.Name(), id)
	}

	return refuseStopped(id, status.State)
}

func refuseStopped(id string, state models.State) error {
	return fmt.Errorf("sandbox %s is %s: start it again with shard start %s", id, state, id)
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

// runExec picks the stdio the guest process gets. Ctrl-C during either path ends that process and
// nothing else: the cancelled exec signals the one guest pid, and only stop ends a sandbox.
func (a App) runExec(ctx context.Context, d *deps, provider models.Provider, opts execOptions, spec models.ExecSpec) (models.ExitStatus, error) {
	if opts.tty {
		return a.execOnTerminal(ctx, d, provider, opts, spec)
	}

	if opts.interactive {
		spec.Stdin = d.stdin()
	}
	spec.Stdout, spec.Stderr = d.stdout(), d.stderr()

	return provider.Exec(ctx, opts.id, spec)
}

// execOnTerminal gives the guest a pty replica and puts this terminal into raw mode, so a keystroke
// reaches the guest untouched. The restore runs on every path out of here, a panic included.
func (a App) execOnTerminal(ctx context.Context, d *deps, provider models.Provider, opts execOptions, spec models.ExecSpec) (status models.ExitStatus, err error) {
	pair, err := pty.Open()
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, pair.Close()) }()

	size, err := pty.SizeOf(d.stdin())
	if err != nil {
		return models.ExitStatus{}, err
	}
	if err := pair.Resize(size); err != nil {
		return models.ExitStatus{}, err
	}

	restore, err := pty.MakeRaw(d.stdin())
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, restore()) }()

	stop := forwardResize(pair, d.stdin(), a.warn)
	defer stop()

	// A terminal carries one stream, so all three fds are the same file.
	spec.Stdin, spec.Stdout, spec.Stderr = pair.Replica, pair.Replica, pair.Replica

	drained := pump(pair, d, a.warn)

	status, err = provider.Exec(ctx, opts.id, spec)

	// Our copy of the replica is what keeps the master readable, so the output drains only after it goes.
	closeErr := pair.Replica.Close()
	// The deferred Close takes the master alone now, because a second close of the replica is an error.
	pair.Replica = nil
	if closeErr != nil {
		return status, errors.Join(err, fmt.Errorf("close the pseudo terminal replica: %w", closeErr))
	}

	// A process the command left behind holds the replica too, and then nothing ever ends the copy.
	// The status is already in hand, so the wait is bounded: the terminal is raw until this returns.
	select {
	case <-drained:
	case <-time.After(drainBudget):
	}

	return status, err
}

// pump moves bytes both ways and reports when the guest side has nothing left to say. The keyboard
// copier is left running: it blocks on a terminal shard does not own, and the process is about to end.
func pump(pair *pty.Pty, d *deps, warn func(string)) <-chan struct{} {
	go func() {
		if _, err := io.Copy(pair.Master, d.stdin()); err != nil {
			warn(fmt.Sprintf("the keyboard stopped reaching the command: %v", err))
		}
	}()

	drained := make(chan struct{})
	go func() {
		defer close(drained)

		// The master reports the replica's last close as an error, and that is the normal end of a session.
		if _, err := io.Copy(d.stdout(), pair.Master); err != nil && !errors.Is(err, syscall.EIO) {
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
	exited := make(chan struct{})
	go func() {
		defer close(exited)

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
		// The caller closes the master next, and an ioctl on a descriptor that is gone goes to whatever took it.
		<-exited
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
