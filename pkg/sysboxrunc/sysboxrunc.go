// Package sysboxrunc drives the sysbox-runc binary. It knows the flags, the subcommands and the state
// JSON, and nothing about sandboxes. It is runc's command line, so there is no checkpoint and no
// restore: the fork dropped both (nestybox/sysbox#715), and the provider refuses what needs them.
package sysboxrunc

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

// ErrNotFound is what a verb aimed at a container sysbox-runc does not hold returns. Match it with errors.Is.
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

// Status is the container status sysbox-runc reports. It is the OCI set, and stopped is the terminal one.
type Status string

const (
	StatusCreating Status = "creating"
	StatusCreated  Status = "created"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusStopped  Status = "stopped"
)

// State is what sysbox-runc state prints. PID is 0 once the container is stopped.
type State struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	PID    int    `json:"pid"`
	Bundle string `json:"bundle"`
}

// Runner runs one sysbox-runc root. Every container under it is reachable from any shard process,
// so nothing here is held in memory between commands.
type Runner struct {
	binary string
	root   string
}

// Option configures a Runner.
type Option func(*Runner)

// WithBinary points at a sysbox-runc other than the one on PATH.
func WithBinary(path string) Option {
	return func(r *Runner) { r.binary = path }
}

// New prepares the sysbox-runc root, which is /var/lib/shard/sysbox-runc on the box.
func New(root string, opts ...Option) (*Runner, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("the sysbox-runc root must be an absolute path, got %q", root)
	}

	r := &Runner{binary: "sysbox-runc", root: root}
	for _, opt := range opts {
		opt(r)
	}

	if _, err := exec.LookPath(r.binary); err != nil {
		return nil, fmt.Errorf("find %s: %w", r.binary, err)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create the sysbox-runc root %s: %w", root, err)
	}

	return r, nil
}

// Root is where sysbox-runc keeps its own container state, which outlives the process that created it.
func (r *Runner) Root() string { return r.root }

// CreateOptions carries the fds the guest inherits. sysbox-runc create hands them over and exits; the
// container keeps them, which is what makes the guest output stream after the command has returned.
type CreateOptions struct {
	Bundle string
	Stdout *os.File
	Stderr *os.File
}

// Create prepares the container. Nothing in the guest runs until Start. The netns it joins is the one
// config.json names: runc has no network flag of its own.
func (r *Runner) Create(ctx context.Context, id string, opts CreateOptions) error {
	if opts.Bundle == "" {
		return errors.New("no bundle: sysbox-runc create has nothing to run")
	}

	// The caller may delete the log the moment this fails, so the diagnostics have to travel in the error.
	start, err := logEnd(opts.Stderr)
	if err != nil {
		return fmt.Errorf("sysbox-runc create %s: %w", id, err)
	}

	cmd := r.command(ctx, "create", "--bundle", opts.Bundle, id)
	cmd.Stdout, cmd.Stderr = opts.Stdout, opts.Stderr

	if err := cmd.Run(); err != nil {
		// Our own cancellation killed it, so what it did not print says nothing about why.
		if ctx.Err() != nil {
			return fmt.Errorf("sysbox-runc create %s: %w: %w", id, err, ctx.Err())
		}

		return fmt.Errorf("sysbox-runc create %s: %w%s", id, err, diagnostics(opts.Stderr, start))
	}

	return nil
}

// ExecOptions is one process in a container that already runs. It is never the entrypoint, so it has
// no supervisor and its exit ends nothing.
type ExecOptions struct {
	Argv    []string
	Env     []string
	WorkDir string
	// User is uid[:gid], and the caller resolves it: config.json's process user is the supervisor's.
	User string
	// Groups is the supplementary set that goes with User.
	Groups []uint32
	// RootFS is the container's live tree on the host. When set, Exec looks the command up in it
	// before anything runs, which is the only way to tell a command that never ran from one that did.
	RootFS string
	// TTY says the three files below are one pty replica, which is the only way the guest gets a terminal.
	TTY bool
	// The files the guest process gets. They are files, not pipes, so a pty replica passes straight through.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}

// Exec runs a command in a running container and returns the code it exited with, which is no failure
// of this driver. A command sysbox-runc cannot start exits 1 with the reason on the guest's stderr and
// nothing in its own log, so the lookup against RootFS is what catches it; without one, a missing
// command reads as a command that exited 1. The caller checks the container is running first.
func (r *Runner) Exec(ctx context.Context, id string, opts ExecOptions) (code int, err error) {
	if len(opts.Argv) == 0 {
		return 0, errors.New("no command: sysbox-runc exec has nothing to run")
	}

	if opts.RootFS != "" {
		if err := LookPath(opts.RootFS, opts.WorkDir, pathOf(opts.Env), opts.Argv[0]); err != nil {
			return 0, fmt.Errorf("sysbox-runc exec %s: %w", id, err)
		}
	}

	dir, err := os.MkdirTemp("", "shard-exec-")
	if err != nil {
		return 0, fmt.Errorf("create a directory for the exec pid file: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	pidFile := filepath.Join(dir, "pid")

	cmd := r.command(ctx, execArgs(id, pidFile, opts)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr

	if opts.TTY {
		// runc gives the guest a terminal only when its own stdio is one it controls.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}

	// Killing sysbox-runc exec leaves the guest process running, so a cancellation has to reach into the container.
	cmd.Cancel = func() error { return r.interrupt(cmd, id, pidFile) }

	if err := cmd.Run(); err != nil {
		// A cancelled call says nothing about how the command would have ended.
		if ctx.Err() != nil {
			return 0, fmt.Errorf("sysbox-runc exec %s: %w", id, ctx.Err())
		}

		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// A driver something else killed says nothing about the guest process, so it is not an exit code.
			if !exit.Exited() {
				return 0, fmt.Errorf("sysbox-runc exec %s was ended by a signal: %w", id, err)
			}

			return exit.ExitCode(), nil
		}

		return 0, fmt.Errorf("sysbox-runc exec %s: %w", id, err)
	}

	return 0, nil
}

// execArgs spells one sysbox-runc exec. The flags precede the id, and everything after it is the command.
func execArgs(id, pidFile string, opts ExecOptions) []string {
	args := []string{"exec", "--pid-file", pidFile}

	if opts.WorkDir != "" {
		args = append(args, "--cwd", opts.WorkDir)
	}
	if opts.User != "" {
		args = append(args, "--user", opts.User)
		for _, gid := range opts.Groups {
			args = append(args, "--additional-gids", strconv.FormatUint(uint64(gid), 10))
		}
	}
	for _, entry := range opts.Env {
		args = append(args, "--env", entry)
	}
	if opts.TTY {
		args = append(args, "--tty")
	}

	return append(append(args, id), opts.Argv...)
}

// pathOf is the PATH the guest command is looked up on, which is the one the exec is given.
func pathOf(env []string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			return value
		}
	}

	return ""
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

// readPID reads the guest pid sysbox-runc wrote, which is the only handle a signal into the container has.
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

// Pause freezes every task in the container. Nothing writes its memory out: sysbox-runc has no checkpoint.
func (r *Runner) Pause(ctx context.Context, id string) error {
	return r.run(ctx, io.Discard, "pause", id)
}

// Resume thaws a paused container. It is the runc verb, not the shard one, which restores a snapshot.
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

// Delete drops sysbox-runc's own state for the container. Until it runs, a stopped container still exists.
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
			return fmt.Errorf("sysbox-runc %s: %w", strings.Join(args, " "), ctx.Err())
		}

		message := strings.TrimSpace(stderr.String())

		return fmt.Errorf("sysbox-runc %s: %w: %s", strings.Join(args, " "), sentinel(message, err), message)
	}

	return nil
}

func (r *Runner) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.binary, append([]string{"--root", r.root}, args...)...)
	// Without this a cancelled call still blocks until every child sysbox-runc forked closes the pipes it inherited.
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
