// Package runsc drives the runsc binary. It knows the flags, the subcommands and the state JSON,
// and nothing about sandboxes: no Docker and no containerd, because gVisor checkpoint and restore
// is unreachable through the containerd shim (containerd#12280).
package runsc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNotFound is what a verb aimed at a container runsc does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("no such container")

// ErrNotRunning is what a kill of an already dead sandbox returns, which a stop must not treat as a failure.
var ErrNotRunning = errors.New("the sandbox is not running")

// runsc reports every missing container through the same load failure, so its text is the only signal.
// waitDelay bounds how long a cancelled call waits for the output pipes after the kill signal.
const waitDelay = 2 * time.Second

// diagnosticTail bounds what a failed create quotes back, because the guest shares that file with it.
const diagnosticTail = 4 << 10

// signalBudget bounds the signal a cancelled exec sends into the sandbox, which runs off its own context.
const signalBudget = 5 * time.Second

const (
	notFoundMessage   = "loading container: file does not exist"
	notRunningMessage = "sandbox is not running"
	// The kernel gives runsc EACCES for a file it may not execute and ENOENT for everything else,
	// a missing interpreter of a script included, which is what a shell answers 126 and 127 for.
	notExecutableMessage = "permission denied"
	// runsc wraps a refusal in the call that hit it, and only the innermost part of that says why.
	innermostMessage = "failed to "
)

// Status is the container status runsc reports. It is the OCI set, and stopped is the terminal one.
type Status string

const (
	StatusCreating Status = "creating"
	StatusCreated  Status = "created"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusStopped  Status = "stopped"
)

// State is what runsc state prints. PID is -1 once the container is stopped.
type State struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	PID    int    `json:"pid"`
	Bundle string `json:"bundle"`
}

// Runner runs one runsc root. Every container under it is reachable from any shard process, so
// nothing here is held in memory between commands.
type Runner struct {
	binary  string
	root    string
	network string
}

// Option configures a Runner.
type Option func(*Runner)

// WithBinary points at a runsc other than the one on PATH.
func WithBinary(path string) Option {
	return func(r *Runner) { r.binary = path }
}

// The network modes runsc accepts. Sandbox is netstack over the namespace's interfaces, which is
// what a sandbox with a veth needs; none leaves it with loopback alone.
const (
	NetworkNone    = "none"
	NetworkSandbox = "sandbox"
	NetworkHost    = "host"
)

// WithNetwork picks the sandbox network mode. Sandbox is what a sandbox with an allocated netns needs.
func WithNetwork(mode string) Option {
	return func(r *Runner) { r.network = mode }
}

// New prepares the runsc root, which is /var/lib/shard/runsc on the box.
func New(root string, opts ...Option) (*Runner, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("the runsc root must be an absolute path, got %q", root)
	}

	r := &Runner{binary: "runsc", root: root, network: NetworkNone}
	for _, opt := range opts {
		opt(r)
	}

	if _, err := exec.LookPath(r.binary); err != nil {
		return nil, fmt.Errorf("find %s: %w", r.binary, err)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create the runsc root %s: %w", root, err)
	}

	return r, nil
}

// Root is where runsc keeps its own container state, which outlives the process that created it.
func (r *Runner) Root() string { return r.root }

// CreateOptions carries the fds the guest inherits. runsc create hands them over and exits; the
// sandbox keeps them, which is what makes the guest output stream after the command has returned.
type CreateOptions struct {
	Bundle string
	Stdout *os.File
	Stderr *os.File
}

// Create prepares the container. Nothing in the guest runs until Start.
func (r *Runner) Create(ctx context.Context, id string, opts CreateOptions) error {
	if opts.Bundle == "" {
		return errors.New("no bundle: runsc create has nothing to run")
	}

	// The caller may delete the log the moment this fails, so the diagnostics have to travel in the error.
	start, err := logEnd(opts.Stderr)
	if err != nil {
		return fmt.Errorf("runsc create %s: %w", id, err)
	}

	cmd := r.command(ctx, "create", "--bundle", opts.Bundle, id)
	cmd.Stdout, cmd.Stderr = opts.Stdout, opts.Stderr

	if err := cmd.Run(); err != nil {
		// Our own cancellation killed it, so what it did not print says nothing about why.
		if ctx.Err() != nil {
			return fmt.Errorf("runsc create %s: %w: %w", id, err, ctx.Err())
		}

		return fmt.Errorf("runsc create %s: %w%s", id, err, diagnostics(opts.Stderr, start))
	}

	return nil
}

// ExecOptions is one process in a sandbox that already runs. It is never the entrypoint, so it has
// no supervisor and its exit ends nothing.
type ExecOptions struct {
	Argv    []string
	Env     []string
	WorkDir string
	// User is uid[:gid], and the caller resolves it: config.json's process user is the supervisor's.
	User string
	// TTY says the three files below are one pty replica, which is the only way the guest gets a terminal.
	TTY bool
	// The files the guest process gets. They are files, not pipes, so a pty replica passes straight through.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}

// Exec runs a command in a running sandbox and returns the code it exited with, which is no failure
// of this driver. runsc writes its own startup failures to the same stderr the guest gets, so an
// exit code alone cannot tell the two apart; the caller checks the sandbox is running first.
func (r *Runner) Exec(ctx context.Context, id string, opts ExecOptions) (code int, err error) {
	if len(opts.Argv) == 0 {
		return 0, errors.New("no command: runsc exec has nothing to run")
	}

	dir, err := os.MkdirTemp("", "shard-exec-")
	if err != nil {
		return 0, fmt.Errorf("create a directory for the exec pid file: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	pidFile := filepath.Join(dir, "pid")
	logFile := filepath.Join(dir, "log")

	// --log is global, and r.command puts what it is given after its own globals and before the subcommand.
	cmd := r.command(ctx, append([]string{"--log", logFile, "--log-format=json"}, execArgs(id, pidFile, opts)...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr

	if opts.TTY {
		// runsc gives the guest a terminal only when its own stdio is one it controls.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}

	// Killing runsc exec leaves the guest process running, so a cancellation has to reach into the sandbox.
	cmd.Cancel = func() error { return r.interrupt(cmd, id, pidFile) }

	if err := cmd.Run(); err != nil {
		// A cancelled call says nothing about how the command would have ended.
		if ctx.Err() != nil {
			return 0, fmt.Errorf("runsc exec %s: %w", id, ctx.Err())
		}

		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// A driver something else killed says nothing about the guest process, so it is not an exit code.
			if !exit.Exited() {
				return 0, fmt.Errorf("runsc exec %s was ended by a signal: %w", id, err)
			}

			// An exit code is the command's unless both say it never ran: the pid file, which lands
			// as soon as the guest process forks, and a refusal runsc logged.
			_, perr := readPID(pidFile)
			reason, rerr := logReason(logFile)
			if perr != nil && rerr == nil {
				return 0, fmt.Errorf("runsc exec %s: %w", id, startFailure(reason))
			}

			return exit.ExitCode(), nil
		}

		return 0, fmt.Errorf("runsc exec %s: %w", id, err)
	}

	return 0, nil
}

// ExecStartError is an exec whose command never ran, which runsc reports as its own exit code 128.
type ExecStartError struct {
	// Reason is runsc's own words, taken from the log this one call wrote.
	Reason string
	// NotExecutable separates a command the sandbox found and could not run from one it never found.
	NotExecutable bool
}

func (e *ExecStartError) Error() string { return e.Reason }

// startFailure names the refusal the kernel gave runsc, which is all a shell needs to tell a command
// it cannot find from one it may not run.
func startFailure(reason string) error {
	if at := strings.LastIndex(reason, innermostMessage); at >= 0 {
		reason = reason[at:]
	}

	return &ExecStartError{Reason: reason, NotExecutable: strings.Contains(reason, notExecutableMessage)}
}

// logReason keeps the last error runsc logged, which is the refusal that ended the call. runsc writes
// the same words to the guest's stderr, so this log is the only copy shard can read back on its own.
func logReason(path string) (string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	var reason string
	for line := range strings.Lines(strings.TrimSpace(string(blob))) {
		var entry struct {
			Message string `json:"msg"`
			Level   string `json:"level"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return "", fmt.Errorf("decode %s: %w", path, err)
		}
		if entry.Level == "error" {
			reason = entry.Message
		}
	}

	if reason == "" {
		return "", errors.New("runsc logged nothing")
	}

	return reason, nil
}

// execArgs spells one runsc exec. The flags precede the id, and everything after it is the command.
func execArgs(id, pidFile string, opts ExecOptions) []string {
	args := []string{"exec", "--internal-pid-file", pidFile}

	if opts.WorkDir != "" {
		args = append(args, "--cwd", opts.WorkDir)
	}
	if opts.User != "" {
		args = append(args, "--user", opts.User)
	}
	for _, entry := range opts.Env {
		args = append(args, "--env", entry)
	}

	return append(append(args, id), opts.Argv...)
}

// interrupt ends the guest process a cancelled exec started. It is SIGKILL because nothing above this
// can wait out a process that refuses to leave, and it never touches the sandbox: only Stop ends one.
func (r *Runner) interrupt(cmd *exec.Cmd, id, pidFile string) error {
	pid, err := readPID(pidFile)
	// The file lands as soon as the guest process forks, so an unreadable one means none did.
	if err != nil {
		return cmd.Process.Kill()
	}

	ctx, cancel := context.WithTimeout(context.Background(), signalBudget)
	defer cancel()

	if err := r.run(ctx, io.Discard, "kill", "--pid", strconv.Itoa(pid), id, "KILL"); err != nil {
		return errors.Join(err, cmd.Process.Kill())
	}

	return nil
}

// readPID reads the guest pid runsc wrote, which is the only handle a signal into the sandbox has.
func readPID(path string) (int, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil {
		return 0, fmt.Errorf("the exec pid file %s holds %q: %w", path, blob, err)
	}

	return pid, nil
}

// Start runs the container's process, which is the supervisor shard-init.
func (r *Runner) Start(ctx context.Context, id string) error {
	return r.run(ctx, io.Discard, "start", id)
}

// Kill signals the container. all reaches every process in it; without it only PID 1 is signalled.
func (r *Runner) Kill(ctx context.Context, id, signal string, all bool) error {
	args := []string{"kill"}
	if all {
		args = append(args, "--all")
	}

	return r.run(ctx, io.Discard, append(args, id, signal)...)
}

// Delete drops runsc's own state for the container. Until it runs, a stopped container still exists.
func (r *Runner) Delete(ctx context.Context, id string, force bool) error {
	args := []string{"delete"}
	if force {
		args = append(args, "--force")
	}

	return r.run(ctx, io.Discard, append(args, id)...)
}

// State asks the substrate what the container is doing. It never consults a record.
func (r *Runner) State(ctx context.Context, id string) (State, error) {
	var out bytes.Buffer
	if err := r.run(ctx, &out, "state", id); err != nil {
		return State{}, err
	}

	var state State
	if err := json.Unmarshal(out.Bytes(), &state); err != nil {
		return State{}, fmt.Errorf("decode the state of %s: %w", id, err)
	}

	return state, nil
}

// run collects stderr so a failure can be classified, and leaves stdout to the caller.
func (r *Runner) run(ctx context.Context, stdout io.Writer, args ...string) error {
	var stderr bytes.Buffer

	cmd := r.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = stdout, &stderr

	if err := cmd.Run(); err != nil {
		// A cancelled call says nothing about the sandbox, so report the context and not what the kill looked like.
		if ctx.Err() != nil {
			return fmt.Errorf("runsc %s: %w", strings.Join(args, " "), ctx.Err())
		}

		message := strings.TrimSpace(stderr.String())

		return fmt.Errorf("runsc %s: %w: %s", strings.Join(args, " "), sentinel(message, err), message)
	}

	return nil
}

func (r *Runner) command(ctx context.Context, args ...string) *exec.Cmd {
	// --overlay2=none because runsc otherwise writes into a filestore it throws away on a stop, and
	// the sandbox's writable layer is the overlayfs mount services/bundle owns.
	global := []string{"--root", r.root, "--network=" + r.network, "--overlay2=none"}

	cmd := exec.CommandContext(ctx, r.binary, append(global, args...)...)
	// Without this a cancelled call still blocks until every child runsc forked closes the pipes it inherited.
	cmd.WaitDelay = waitDelay

	return cmd
}

// sentinel turns the two failures a caller must act on into errors it can match.
func sentinel(message string, err error) error {
	if strings.Contains(message, notFoundMessage) {
		return ErrNotFound
	}
	if strings.Contains(message, notRunningMessage) {
		return ErrNotRunning
	}

	return err
}

// logEnd is where a create's own output begins, because the file it writes to already holds the
// guest's output from an earlier run.
func logEnd(f *os.File) (int64, error) {
	if f == nil {
		return 0, nil
	}

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", f.Name(), err)
	}

	return info.Size(), nil
}

// diagnostics quotes what a failed create printed, as the suffix of the error that reports it.
func diagnostics(f *os.File, offset int64) string {
	if f == nil {
		return ": shard captured no output from it"
	}

	blob, err := readTail(f.Name(), offset)
	if err != nil {
		return fmt.Sprintf(": its diagnostics were unreadable: %v", err)
	}

	text := strings.TrimSpace(string(blob))
	if text == "" {
		return ": it printed nothing"
	}

	return ": " + text
}

// readTail reads the file from offset, keeping the last diagnosticTail bytes of it.
func readTail(path string, offset int64) (blob []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	if info.Size()-offset > diagnosticTail {
		offset = info.Size() - diagnosticTail
	}

	return io.ReadAll(io.NewSectionReader(f, offset, diagnosticTail))
}
