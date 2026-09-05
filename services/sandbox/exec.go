package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// drainBudget is how long the command's last output may take to arrive once the command itself is gone.
const drainBudget = 2 * time.Second

// ExecRequest is one command to run in a sandbox that already runs. It is the body of POST /v0/sandboxes/{id}/exec.
type ExecRequest struct {
	Command []string `json:"command"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	// Stdin says the client will type at the command; without it the command reads nothing at all.
	Stdin bool `json:"stdin,omitempty"`
	// TTY gives the command a terminal in the guest, which the daemon holds the other end of.
	TTY bool `json:"tty,omitempty"`
	// Size is the terminal the command starts on, and a resize replaces it.
	Size TerminalSize `json:"size,omitzero"`
}

// TerminalSize is a terminal window in character cells. It is the body of the resize route too.
type TerminalSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// Streams is where one exec's stdio goes. The caller owns them: a nil Stdin is a command that reads nothing.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Started is called with the exec id before the command runs, and its error ends the exec.
	Started func(execID string) error
	// Warn reports what the keyboard copier cannot return, because nothing waits for that goroutine.
	Warn func(message string)
}

// UnavailableError is a sandbox no command can run in, because the substrate no longer holds it.
type UnavailableError struct {
	ID string
	// Why is what became of the sandbox, and Fix what the operator does about it.
	Why string
	Fix string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("sandbox %s %s: %s", e.ID, e.Why, e.Fix)
}

// execSession is one exec on a terminal, kept until it ends so a resize can find its pty.
type execSession struct {
	sandboxID string
	pair      *pty.Pty
}

// Exec runs one command in a sandbox that is already up. It is never the entrypoint, and its exit
// ends nothing: only stop ends a sandbox.
func (s *Service) Exec(ctx context.Context, ref string, req ExecRequest, streams Streams) (models.ExitStatus, error) {
	if len(req.Command) == 0 {
		return models.ExitStatus{}, &RequestError{Err: errors.New("the request names no command to run")}
	}

	id, err := s.readyForExec(ctx, ref)
	if err != nil {
		return models.ExitStatus{}, err
	}

	execID, err := newExecID()
	if err != nil {
		return models.ExitStatus{}, err
	}

	if req.TTY {
		return s.execOnTerminal(ctx, id, execID, req, streams)
	}

	return s.execOnPipes(ctx, id, execID, req, streams)
}

// readyForExec resolves the reference and refuses a sandbox no command can run in. The record
// answers for an id nobody created, and the substrate for the state, because a record saying
// running outlives a host restart.
func (s *Service) readyForExec(ctx context.Context, ref string) (string, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", err
	}

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return "", err
	}

	// A record that says stopped outranks the oom count the cgroup kept, and a paused one never has a cgroup.
	if sb.State == models.StateStopped {
		return "", &StateError{ID: id, State: sb.State, Fix: "start it again with shard start " + id}
	}

	// The provider holds nothing of a paused sandbox, and gone is the wrong word for one a resume brings back.
	if sb.State == models.StatePaused {
		return "", &StateError{ID: id, State: sb.State, Fix: "resume it with shard resume " + id}
	}

	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		return "", err
	}
	if status.Alive() {
		return id, nil
	}

	// The exit file records a 137 for this, which is what a plain kill -9 records too, so the reason
	// is named here or an operator never learns it.
	if status.OOMKilled {
		return "", &UnavailableError{ID: id, Why: "ran out of memory and the host ended it", Fix: fmt.Sprintf("remove it with shard rm %s and create another with a larger --memory", id)}
	}

	if !status.Exists {
		return "", &UnavailableError{ID: id, Why: "is gone from " + s.cfg.Provider.Name(), Fix: fmt.Sprintf("remove it with shard rm %s and create another", id)}
	}

	return "", &StateError{ID: id, State: status.State, Fix: "start it again with shard start " + id}
}

// execOnPipes gives the guest process one pipe per stream, because the substrate hands it files.
func (s *Service) execOnPipes(ctx context.Context, id, execID string, req ExecRequest, streams Streams) (status models.ExitStatus, err error) {
	spec := specOf(req)

	if streams.Stdin != nil {
		reader, writer, err := os.Pipe()
		if err != nil {
			return models.ExitStatus{}, fmt.Errorf("open the stdin pipe of the exec in sandbox %s: %w", id, err)
		}
		defer func() { err = errors.Join(err, reader.Close()) }()

		spec.Stdin = reader
		// Nothing waits for this copier: it blocks on a client that may say nothing until the exec ends.
		go func() {
			warn(streams.Warn, errors.Join(copyStream(writer, streams.Stdin), writer.Close()), "the keyboard stopped reaching the command")
		}()
	}

	out, drainOut, err := outputPipe(id, "stdout", streams.Stdout)
	if err != nil {
		return models.ExitStatus{}, err
	}
	spec.Stdout = out

	errOut, drainErr, err := outputPipe(id, "stderr", streams.Stderr)
	if err != nil {
		return models.ExitStatus{}, errors.Join(err, out.Close())
	}
	spec.Stderr = errOut

	if err := started(streams, execID); err != nil {
		return models.ExitStatus{}, errors.Join(err, out.Close(), errOut.Close())
	}

	status, execErr := s.cfg.Provider.Exec(ctx, id, spec)

	// Our copy of each write end is what keeps its pipe readable, so the output drains only after they go.
	closeErr := errors.Join(out.Close(), errOut.Close())

	return status, errors.Join(execErr, closeErr, <-drainOut, <-drainErr)
}

// outputPipe copies one of the guest's streams to the client and reports what stopped the copy.
func outputPipe(id, name string, w io.Writer) (*os.File, <-chan error, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("open the %s pipe of the exec in sandbox %s: %w", name, id, err)
	}

	drained := make(chan error, 1)
	go func() {
		drained <- errors.Join(copyStream(w, reader), reader.Close())
	}()

	return writer, drained, nil
}

// execOnTerminal gives the guest a pty replica and merges what it writes into the client's stdout: a
// terminal carries one stream, so there is no stderr to keep apart.
func (s *Service) execOnTerminal(ctx context.Context, id, execID string, req ExecRequest, streams Streams) (status models.ExitStatus, err error) {
	pair, err := pty.Open()
	if err != nil {
		return models.ExitStatus{}, err
	}
	defer func() { err = errors.Join(err, pair.Close()) }()

	if req.Size.Rows != 0 && req.Size.Cols != 0 {
		if err := pair.Resize(pty.Size{Rows: req.Size.Rows, Cols: req.Size.Cols}); err != nil {
			return models.ExitStatus{}, err
		}
	}

	s.holdExec(execID, &execSession{sandboxID: id, pair: pair})
	defer s.dropExec(execID)

	spec := specOf(req)
	// A terminal carries one stream, so all three fds are the same file.
	spec.Stdin, spec.Stdout, spec.Stderr = pair.Replica, pair.Replica, pair.Replica

	if streams.Stdin != nil {
		go func() {
			warn(streams.Warn, copyStream(pair.Master, streams.Stdin), "the keyboard stopped reaching the command")
		}()
	}

	drained := make(chan error, 1)
	go func() {
		drained <- copyStream(streams.Stdout, pair.Master)
	}()

	if err := started(streams, execID); err != nil {
		return models.ExitStatus{}, err
	}

	status, err = s.cfg.Provider.Exec(ctx, id, spec)

	// Our copy of the replica is what keeps the master readable, so the output drains only after it goes.
	closeErr := pair.Replica.Close()
	// The deferred Close takes the master alone now, because a second close of the replica is an error.
	pair.Replica = nil
	if closeErr != nil {
		return status, errors.Join(err, fmt.Errorf("close the pseudo terminal replica: %w", closeErr))
	}

	// A process the command left behind holds the replica too, and then nothing ever ends the copy.
	select {
	case drainErr := <-drained:
		return status, errors.Join(err, drainErr)
	case <-time.After(drainBudget):
	}

	return status, err
}

// ResizeExec sets the window of one exec that runs on a terminal, which is what a SIGWINCH forwards.
func (s *Service) ResizeExec(_ context.Context, ref, execID string, size TerminalSize) error {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	s.execMu.Lock()
	session := s.execs[execID]
	s.execMu.Unlock()

	// An exec that already ended is gone, and so is one that belongs to another sandbox.
	if session == nil || session.sandboxID != id {
		return fmt.Errorf("exec %s of sandbox %s: %w", execID, id, sandboxstate.ErrNotFound)
	}

	return session.pair.Resize(pty.Size{Rows: size.Rows, Cols: size.Cols})
}

func (s *Service) holdExec(execID string, session *execSession) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	s.execs[execID] = session
}

func (s *Service) dropExec(execID string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	delete(s.execs, execID)
}

func specOf(req ExecRequest) models.ExecSpec {
	return models.ExecSpec{
		Argv:    req.Command,
		Env:     req.Env,
		WorkDir: req.WorkDir,
		User:    req.User,
		TTY:     req.TTY,
	}
}

// newExecID names one exec for as long as it runs, which is what a resize reaches it by.
func newExecID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes for an exec id: %w", err)
	}

	return fmt.Sprintf("%x", b), nil
}

// copyStream drops the errors that are how a session ends: a pipe whose other end went, the EIO a
// pty master reports once the replica is closed, and a client that hung up.
func copyStream(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)

	switch {
	case err == nil,
		errors.Is(err, io.EOF),
		errors.Is(err, os.ErrClosed),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.EIO):
		return nil
	}

	return err
}

// started tells the caller the exec exists, so it can name it to the client before the command runs.
func started(streams Streams, execID string) error {
	if streams.Started == nil {
		return nil
	}

	return streams.Started(execID)
}

// warn reports what no caller waits for, because the keyboard copier outlives the command it fed.
func warn(report func(string), err error, what string) {
	if err == nil || report == nil {
		return
	}

	report(fmt.Sprintf("%s: %v", what, err))
}
