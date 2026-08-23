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
	"strings"
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

const (
	notFoundMessage   = "loading container: file does not exist"
	notRunningMessage = "sandbox is not running"
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
