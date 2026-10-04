// Package runc drives the runc command line: the flags, the subcommands and the state JSON, and
// nothing about sandboxes. sysbox-runc shares it, so a provider names its binary with WithBinary.
// There is no checkpoint and no restore: sysbox-runc dropped both (nestybox/sysbox#715).
package runc

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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/launch"
)

// ErrNotFound is what a verb aimed at a container runc does not hold returns. Match it with errors.Is.
var ErrNotFound = errors.New("no such container")

// ErrNotRunning is what a kill of an already dead container returns, which a stop must not treat as a failure.
var ErrNotRunning = errors.New("the container is not running")

// waitDelay bounds how long a cancelled call waits for the output pipes after the kill signal.
const waitDelay = 2 * time.Second

// diagnosticTail bounds what a failed create quotes back, because the guest shares that file with it.
const diagnosticTail = 4 << 10

const (
	notFoundMessage   = "does not exist"
	notRunningMessage = "container not running"
)

// Status is the container status runc reports. It is the OCI set, and stopped is the terminal one.
type Status string

const (
	StatusCreating Status = "creating"
	StatusCreated  Status = "created"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusStopped  Status = "stopped"
)

// State is what runc state prints. PID is 0 once the container is stopped.
type State struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	PID    int    `json:"pid"`
	Bundle string `json:"bundle"`
}

// Runner owns one runc root and the pinned handles of its active execs.
type Runner struct {
	binary       string
	root         string
	execDir      string
	noNewKeyring bool
	execMu       sync.Mutex
	nextExec     int
	execs        map[int]execHandle
}

// Option configures a Runner.
type Option func(*Runner)

// WithBinary points at a runc other than the one on PATH.
func WithBinary(path string) Option {
	return func(r *Runner) { r.binary = path }
}

// WithExecDir keeps each exec's scratch under dir, off the runc root that it scans, so a restarted daemon can sweep it.
func WithExecDir(dir string) Option {
	return func(r *Runner) { r.execDir = dir }
}

// WithNoNewKeyring has create make no session keyring, which would spend one key of the quota per container.
func WithNoNewKeyring() Option {
	return func(r *Runner) { r.noNewKeyring = true }
}

// New prepares the runc root, which is /var/lib/shard/runc on the box.
func New(root string, opts ...Option) (*Runner, error) {
	r := &Runner{binary: "runc", root: root}
	for _, opt := range opts {
		opt(r)
	}

	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("the %s root must be an absolute path, got %q", r.name(), root)
	}

	if _, err := exec.LookPath(r.binary); err != nil {
		return nil, fmt.Errorf("find %s: %w", r.binary, err)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create the %s root %s: %w", r.name(), root, err)
	}

	if r.execDir != "" {
		if err := os.MkdirAll(r.execDir, 0o700); err != nil {
			return nil, fmt.Errorf("create the exec directory %s: %w", r.execDir, err)
		}
	}

	return r, nil
}

// name is the binary as the provider named it, so a sysbox-runc failure says sysbox-runc.
func (r *Runner) name() string { return filepath.Base(r.binary) }

// Root is where runc keeps its own container state, which outlives the process that created it.
func (r *Runner) Root() string { return r.root }

// CreateOptions carries the fds the guest inherits. runc create hands them over and exits; the
// container keeps them, which is what makes the guest output stream after the command has returned.
type CreateOptions struct {
	Bundle string
	Stdout *os.File
	Stderr *os.File
	// Stdin is the guest's fd 0. shard-init reports the entrypoint exit on it, a channel the guest cannot reach.
	Stdin *os.File
}

// Create prepares the container. Nothing in the guest runs until Start. The netns it joins is the one
// config.json names: runc has no network flag of its own.
func (r *Runner) Create(ctx context.Context, id string, opts CreateOptions) error {
	if opts.Bundle == "" {
		return fmt.Errorf("no bundle: %s create has nothing to run", r.name())
	}

	// The caller may delete the log the moment this fails, so the diagnostics have to travel in the error.
	start, err := logEnd(opts.Stderr)
	if err != nil {
		return fmt.Errorf("%s create %s: %w", r.name(), id, err)
	}

	cmd := r.command(ctx, createArgs(id, opts.Bundle, r.noNewKeyring)...)
	cmd.Stdout, cmd.Stderr = opts.Stdout, opts.Stderr
	cmd.Stdin = opts.Stdin

	if err := cmd.Run(); err != nil {
		// Our own cancellation killed it, so what it did not print says nothing about why.
		if ctx.Err() != nil {
			return fmt.Errorf("%s create %s: %w: %w", r.name(), id, err, ctx.Err())
		}

		return fmt.Errorf("%s create %s: %w%s", r.name(), id, err, diagnostics(opts.Stderr, start))
	}

	return nil
}

// createArgs spells one runc create. The flags precede the id.
func createArgs(id, bundle string, noNewKeyring bool) []string {
	args := []string{"create", "--bundle", bundle}
	if noNewKeyring {
		args = append(args, "--no-new-keyring")
	}

	return append(args, id)
}

// ExecOptions is one process in a container that already runs. It is never the entrypoint, so it has
// no supervisor and its exit ends nothing.
type ExecOptions struct {
	// Bundle is the directory create was given. Its config.json process is what the exec starts from, as runc's own flags would.
	Bundle  string
	Argv    []string
	Env     []string
	WorkDir string
	// User is uid[:gid], and the caller resolves it: config.json's process user is the supervisor's.
	User string
	// Groups is the supplementary set that goes with User.
	Groups []uint32
	// Launch is the supervisor's guest path; set, the command runs under its launch mode, which proves the execve took.
	Launch string
	// TTY says the three files below are one pty replica, which is the only way the guest gets a terminal.
	TTY bool
	// The files the guest process gets. They are files, not pipes, so a pty replica passes straight through.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	// Report gives Signal an opaque handle backed by the launch pin, so reporting requires Launch.
	Report func(handle int)
}

// Exec needs launch proof because runc uses exit 1 for both a command exit and a launch refusal.
func (r *Runner) Exec(ctx context.Context, id string, opts ExecOptions) (code int, err error) {
	if len(opts.Argv) == 0 {
		return 0, fmt.Errorf("no command: %s exec has nothing to run", r.name())
	}
	if opts.Bundle == "" {
		return 0, fmt.Errorf("no bundle: %s exec has no process to start from", r.name())
	}
	if opts.Report != nil && opts.Launch == "" {
		return 0, fmt.Errorf("%s exec: reporting a process handle requires a launch shim", r.name())
	}

	dir, err := os.MkdirTemp(r.execDir, "shard-exec-")
	if err != nil {
		return 0, fmt.Errorf("create a directory for the exec pid file: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	pidFile := filepath.Join(dir, "pid")
	logFile := filepath.Join(dir, "log")
	processFile := filepath.Join(dir, "process.json")
	if err := writeProcess(processFile, opts); err != nil {
		return 0, fmt.Errorf("%s exec %s: %w", r.name(), id, err)
	}

	var ch *launch.Channel
	if opts.Launch != "" {
		ch, err = launch.Open()
		if err != nil {
			return 0, fmt.Errorf("%s exec %s: %w", r.name(), id, err)
		}
		defer func() { err = errors.Join(err, ch.Close()) }()
	}

	var report func(int)
	if opts.Report != nil {
		handle, err := r.trackExec(id, ch)
		if err != nil {
			return 0, fmt.Errorf("%s exec %s: %w", r.name(), id, err)
		}
		defer r.forgetExec(handle)
		report = func(int) { opts.Report(handle) }
	}

	args := execArgs(id, pidFile, processFile, opts)
	if ch != nil {
		args = append([]string{"--log", logFile}, args...)
	}

	cmd := r.command(ctx, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	if ch != nil {
		cmd.ExtraFiles = []*os.File{ch.Guest()}
	}

	// The driver dies with the daemon, so a restart orphans no runc exec; the guest process is reparented inside the container and outlives both.
	cmd.SysProcAttr = execAttr(opts.TTY)

	// The parent-death signal watches the thread that forked, so this goroutine keeps that thread until the driver ends.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Killing runc exec leaves the guest process running, so a cancellation has to reach into the container.
	cmd.Cancel = func() error { return r.interrupt(cmd, id, pidFile) }
	if ch != nil {
		// The pid file is a bare integer a reuse can hold, so a launch kills only what its trace pinned.
		cmd.Cancel = func() error { return errors.Join(ch.Kill(), cmd.Process.Kill()) }
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("%s exec %s: %w", r.name(), id, err)
	}

	launched := make(chan error, 1)
	if ch != nil {
		// The runtime holds its own copy now, and the shim's end must close with it for a runtime that fails to read as EOF.
		if err := ch.CloseGuest(); err != nil {
			return 0, errors.Join(err, cmd.Cancel(), cmd.Wait())
		}
		go func() { launched <- await(ctx, ch, pidFile, report) }()
	}

	err = cmd.Wait()
	if ch != nil {
		if lerr := <-launched; lerr != nil {
			return 0, r.notLaunched(ctx, id, lerr, err, logFile)
		}
	}

	if err != nil {
		// A cancelled call says nothing about how the command would have ended.
		if ctx.Err() != nil {
			return 0, fmt.Errorf("%s exec %s: %w", r.name(), id, ctx.Err())
		}

		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// A driver something else killed says nothing about the guest process, so it is not an exit code.
			if !exit.Exited() {
				return 0, fmt.Errorf("%s exec %s was ended by a signal: %w", r.name(), id, err)
			}

			return exit.ExitCode(), nil
		}

		return 0, fmt.Errorf("%s exec %s: %w", r.name(), id, err)
	}

	return 0, nil
}

// execArgs spells one runc exec. The process file holds the whole command, so nothing follows the id.
func execArgs(id, pidFile, processFile string, opts ExecOptions) []string {
	args := []string{"exec", "--pid-file", pidFile, "--process", processFile}
	if opts.Launch == "" {
		return append(args, id)
	}

	// The channel is the first fd past stdio, which runc hands the shim as its fd 3.
	return append(args, "--preserve-fds", "1", id)
}

// await reports only after the trace proves the command's execve took.
func await(ctx context.Context, ch *launch.Channel, pidFile string, report func(int)) error {
	pid, err := ch.Await(ctx, func() (int, error) { return readPID(pidFile) })
	if err != nil {
		return err
	}
	if report != nil {
		report(pid)
	}

	return nil
}

// notLaunched names an exec whose command never ran. A runc that failed before the shim says why in its own log.
func (r *Runner) notLaunched(ctx context.Context, id string, launchErr, waitErr error, logFile string) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s exec %s: %w", r.name(), id, ctx.Err())
	}
	if !errors.Is(launchErr, launch.ErrNoShim) {
		return fmt.Errorf("%s exec %s: %w", r.name(), id, launchErr)
	}

	why := "it ended"
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		why = exit.String()
	}

	blob, err := readTail(logFile, 0)
	if err != nil {
		return fmt.Errorf("%s exec %s: %w: %s, and its log was unreadable: %w", r.name(), id, launchErr, why, err)
	}

	return fmt.Errorf("%s exec %s: %w: %s: %s", r.name(), id, launchErr, why, strings.TrimSpace(string(blob)))
}

// interrupt ends the guest process a cancelled exec started. It is SIGKILL because nothing above this
// can wait out a process that refuses to leave, and it never touches the container: only Delete ends one.
func (r *Runner) interrupt(cmd *exec.Cmd, id, pidFile string) error {
	pid, err := readPID(pidFile)
	// The file lands as soon as the guest process forks, so an unreadable one means none did.
	if err != nil {
		return cmd.Process.Kill()
	}

	// The pid file holds a host pid, and runc kill signals PID 1 alone, so the signal goes straight to it.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return errors.Join(fmt.Errorf("kill the exec process %d of %s: %w", pid, id, err), cmd.Process.Kill())
	}

	return nil
}

// Signal accepts only a handle this runner reported for this sandbox's live launch.
func (r *Runner) Signal(_ context.Context, id string, handle int, signal string) error {
	sig, err := signalOf(signal)
	if err != nil {
		return err
	}

	r.execMu.Lock()
	e, ok := r.execs[handle]
	r.execMu.Unlock()
	if !ok || e.id != id {
		return os.ErrProcessDone
	}
	if err := e.channel.Signal(sig); err != nil {
		return fmt.Errorf("send %s to the exec of %s: %w", signal, id, err)
	}

	return nil
}

// signalOf maps the two names the API sends to the guest signals they mean.
func signalOf(name string) (syscall.Signal, error) {
	switch name {
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	}

	return 0, fmt.Errorf("signal %q is not one this driver sends", name)
}

// readPID supplies the host pid whose identity the launch channel proves before execve.
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

// Pause freezes every task in the container. Nothing writes its memory out: this driver has no checkpoint.
func (r *Runner) Pause(ctx context.Context, id string) error {
	return r.run(ctx, io.Discard, "pause", id)
}

// Resume thaws a paused container. It is the runc verb, not the shard one, which restores a checkpoint.
func (r *Runner) Resume(ctx context.Context, id string) error {
	return r.run(ctx, io.Discard, "resume", id)
}

// Kill signals the container. all reaches every process in it; without it only PID 1 is signalled.
func (r *Runner) Kill(ctx context.Context, id, signal string, all bool) error {
	args := []string{"kill"}
	if all {
		args = append(args, "--all")
	}

	return r.run(ctx, io.Discard, append(args, id, signal)...)
}

// Delete drops runc's own state for the container. Until it runs, a stopped container still exists.
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
		// A cancelled call says nothing about the container, so report the context and not what the kill looked like.
		if ctx.Err() != nil {
			return fmt.Errorf("%s %s: %w", r.name(), strings.Join(args, " "), ctx.Err())
		}

		message := strings.TrimSpace(stderr.String())

		return fmt.Errorf("%s %s: %w: %s", r.name(), strings.Join(args, " "), sentinel(message, err), message)
	}

	return nil
}

func (r *Runner) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.binary, append([]string{"--root", r.root}, args...)...)
	// Without this a cancelled call still blocks until every child runc forked closes the pipes it inherited.
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
