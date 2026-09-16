package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// drainBudget is how long the command's last output may take to arrive once the command itself is gone.
const drainBudget = 2 * time.Second

// execBufferCap is how much of one exec's output the daemon keeps for a replay: the oldest bytes go first.
const execBufferCap = 8 << 20

// execIDLen is how many hex characters name an exec, so a list cursor that is not one is refused.
const execIDLen = 16

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

// Streams is where one attach's stdio goes. The caller owns them: a nil Stdin is a client that types nothing.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Started is called with the exec id before the replay begins, and its error ends the attach.
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

// AttachedError is an exec a client already watches: one attach at a time replays and streams its output.
type AttachedError struct {
	ID string
}

func (e *AttachedError) Error() string {
	return fmt.Sprintf("exec %s is already attached: wait for that client to leave", e.ID)
}

// ExecExitedError is a kill of an exec that already ended, which has no process left to signal.
type ExecExitedError struct {
	ID string
}

func (e *ExecExitedError) Error() string {
	return fmt.Sprintf("exec %s has exited: nothing left to signal", e.ID)
}

// ExecRunningError is a delete of an exec still under way, whose buffer the daemon must keep.
type ExecRunningError struct {
	ID string
}

func (e *ExecRunningError) Error() string {
	return fmt.Sprintf("exec %s is still running: kill it or wait for it before you delete it", e.ID)
}

// chunk is one write the guest made, tagged with the stream it came on so a replay keeps them apart.
type chunk struct {
	data   []byte
	stderr bool
}

// execBuffer keeps the last execBufferCap bytes of one exec's output, so any attach replays what it has.
type execBuffer struct {
	mu        sync.Mutex
	chunks    []chunk
	size      int
	dropped   int
	truncated bool
	closed    bool
	changed   chan struct{}
}

func newExecBuffer() *execBuffer {
	return &execBuffer{changed: make(chan struct{})}
}

// append copies one write in and evicts the oldest chunks until the buffer is back under its cap.
func (b *execBuffer) append(data []byte, stderr bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	kept := make([]byte, len(data))
	copy(kept, data)
	b.chunks = append(b.chunks, chunk{data: kept, stderr: stderr})
	b.size += len(kept)

	for b.size > execBufferCap && len(b.chunks) > 1 {
		b.size -= len(b.chunks[0].data)
		b.chunks[0] = chunk{}
		b.chunks = b.chunks[1:]
		b.dropped++
		b.truncated = true
	}

	b.wake()
}

// close says the command ended and no more output will come, so a stream drains and returns.
func (b *execBuffer) close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.closed = true
	b.wake()
}

func (b *execBuffer) isTruncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.truncated
}

// wake replaces the change channel so every waiter unblocks. The caller holds the lock.
func (b *execBuffer) wake() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// stream replays what the buffer holds from the oldest byte, then follows live until the command ends
// or the context is cancelled. A cancelled context is a client that left, not the command ending.
func (b *execBuffer) stream(ctx context.Context, emit func(chunk) error) error {
	next := 0
	for {
		b.mu.Lock()
		if next < b.dropped {
			next = b.dropped
		}
		pending := b.chunks[next-b.dropped:]
		batch := make([]chunk, len(pending))
		copy(batch, pending)
		next += len(batch)
		closed := b.closed
		changed := b.changed
		b.mu.Unlock()

		for _, c := range batch {
			if err := emit(c); err != nil {
				return err
			}
		}

		if closed {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// bufWriter appends what the guest writes to one stream into the exec buffer, tagged with that stream.
type bufWriter struct {
	buf    *execBuffer
	stderr bool
}

func (w bufWriter) Write(p []byte) (int, error) {
	w.buf.append(p, w.stderr)
	return len(p), nil
}

// execSession is one exec from its create to its end. The command runs on execCtx, so a client that
// drops never ends it: only stop and a delete-after-exit cancel that context.
type execSession struct {
	id        string
	sandboxID string
	command   []string
	tty       bool
	stdinReq  bool
	startedAt time.Time

	cancel context.CancelFunc
	done   chan struct{}

	buf *execBuffer

	// pid is the guest process id the provider reported, so a kill signals it; pidSet closes once it is set.
	pidMu   sync.Mutex
	pid     int
	pidOnce sync.Once
	pidSet  chan struct{}

	// stdinW is the write end of a non-tty command's stdin; pair is the terminal of a tty command.
	stdinW    *os.File
	stdinOnce sync.Once
	pair      *pty.Pty

	// attachMu guards attached, so one client at a time replays and streams the output.
	attachMu sync.Mutex
	attached bool

	// The result the command left, set once, under mu, when done closes.
	mu       sync.Mutex
	state    models.ExecState
	exit     *models.ExitStatus
	startErr *models.CommandNotStartedError
	runErr   error
	exitedAt *time.Time
}

// record is the exec as the API answers it, a snapshot of where it is now.
func (e *execSession) record() models.Exec {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec := models.Exec{
		ID:        e.id,
		Sandbox:   e.sandboxID,
		Command:   slices.Clone(e.command),
		State:     e.state,
		StartedAt: e.startedAt,
		Truncated: e.buf.isTruncated(),
	}
	if e.exit != nil {
		exit := *e.exit
		rec.ExitStatus = &exit
	}
	if e.exitedAt != nil {
		at := *e.exitedAt
		rec.ExitedAt = &at
	}

	return rec
}

// exited reports whether the command ended and when. It reads exitedAt without the lock: setResult
// writes it before it closes done, so a closed done makes the read safe.
func (e *execSession) exited() (bool, time.Time) {
	select {
	case <-e.done:
		return true, *e.exitedAt
	default:
		return false, time.Time{}
	}
}

// setPID keeps the pid the provider reported and wakes a kill that waits for it.
func (e *execSession) setPID(pid int) {
	e.pidMu.Lock()
	e.pid = pid
	e.pidMu.Unlock()

	e.pidOnce.Do(func() { close(e.pidSet) })
}

// waitPID blocks until the provider reports the pid, the exec ends, or the caller gives up.
func (e *execSession) waitPID(ctx context.Context) (int, error) {
	select {
	case <-e.pidSet:
		e.pidMu.Lock()
		defer e.pidMu.Unlock()

		return e.pid, nil
	case <-e.done:
		return 0, &ExecExitedError{ID: e.id}
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// setResult records how the command ended and closes done. A command that never ran keeps its shell code.
func (e *execSession) setResult(exit models.ExitStatus, err error) {
	e.mu.Lock()

	now := time.Now().UTC()
	e.state = models.ExecExited
	e.exitedAt = &now

	var notStarted *models.CommandNotStartedError
	switch {
	case errors.As(err, &notStarted):
		e.startErr = notStarted
		e.exit = &models.ExitStatus{Code: notStarted.Code}
	case err != nil:
		e.runErr = err
	default:
		done := exit
		e.exit = &done
	}

	e.mu.Unlock()

	close(e.done)
}

// result is how the command ended, for the attach that answers a live client with it.
func (e *execSession) result() (models.ExitStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch {
	case e.startErr != nil:
		return models.ExitStatus{}, e.startErr
	case e.runErr != nil:
		return models.ExitStatus{}, e.runErr
	case e.exit != nil:
		return *e.exit, nil
	}

	return models.ExitStatus{}, nil
}

// tryAttach claims the one attach slot, so a second watcher is refused rather than racing the first.
func (e *execSession) tryAttach() error {
	e.attachMu.Lock()
	defer e.attachMu.Unlock()

	if e.attached {
		return &AttachedError{ID: e.id}
	}
	e.attached = true

	return nil
}

func (e *execSession) endAttach() {
	e.attachMu.Lock()
	e.attached = false
	e.attachMu.Unlock()
}

// stdinTarget is where a client's keystrokes go: the terminal on a tty, the stdin pipe otherwise.
func (e *execSession) stdinTarget() io.Writer {
	if e.tty {
		return e.pair.Master
	}

	return e.stdinW
}

// closeStdin tells a non-tty command its input has ended. A tty keeps its terminal open across a re-attach.
func (e *execSession) closeStdin() error {
	if e.stdinW == nil {
		return nil
	}

	var err error
	e.stdinOnce.Do(func() { err = e.stdinW.Close() })

	return err
}

// CreateExec starts one command in a sandbox that is up and answers the exec record at once. The command
// runs whether or not a client attaches, and only stop or a delete after it ends forgets it.
func (s *Service) CreateExec(ctx context.Context, ref string, req ExecRequest) (models.Exec, error) {
	if len(req.Command) == 0 {
		return models.Exec{}, &RequestError{Err: errors.New("the request names no command to run")}
	}

	id, err := s.readyForExec(ctx, ref)
	if err != nil {
		return models.Exec{}, err
	}

	execID, err := newExecID()
	if err != nil {
		return models.Exec{}, err
	}

	session, err := s.startExec(id, execID, req)
	if err != nil {
		return models.Exec{}, err
	}

	s.holdExec(execID, session)
	s.capExitedExecs(id)

	return session.record(), nil
}

// startExec opens the command's stdio and runs it in the background, so the create returns straight away.
func (s *Service) startExec(id, execID string, req ExecRequest) (*execSession, error) {
	ctx, cancel := context.WithCancel(context.Background())

	session := &execSession{
		id:        execID,
		sandboxID: id,
		command:   slices.Clone(req.Command),
		tty:       req.TTY,
		stdinReq:  req.Stdin,
		startedAt: time.Now().UTC(),
		cancel:    cancel,
		done:      make(chan struct{}),
		buf:       newExecBuffer(),
		pidSet:    make(chan struct{}),
		state:     models.ExecRunning,
	}

	spec := specOf(req)
	spec.Report = session.setPID

	if req.TTY {
		pair, err := pty.Open()
		if err != nil {
			cancel()
			return nil, err
		}
		if req.Size.Rows != 0 && req.Size.Cols != 0 {
			if err := pair.Resize(pty.Size{Rows: req.Size.Rows, Cols: req.Size.Cols}); err != nil {
				cancel()
				return nil, errors.Join(err, pair.Close())
			}
		}

		session.pair = pair
		// A terminal carries one stream, so all three fds are the same file.
		spec.Stdin, spec.Stdout, spec.Stderr = pair.Replica, pair.Replica, pair.Replica
		go s.runTerminal(ctx, id, session, spec)

		return session, nil
	}

	if req.Stdin {
		reader, writer, err := os.Pipe()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("open the stdin pipe of the exec in sandbox %s: %w", id, err)
		}

		spec.Stdin = reader
		session.stdinW = writer
	}

	go s.runPipes(ctx, id, session, spec)

	return session, nil
}

// runPipes runs a non-tty command, copying its stdout and stderr into the buffer, and records its exit.
func (s *Service) runPipes(ctx context.Context, id string, session *execSession, spec models.ExecSpec) {
	defer session.cancel()

	out, drainOut, err := outputPipe(id, "stdout", bufWriter{buf: session.buf})
	if err != nil {
		s.endPipes(session, spec.Stdin, models.ExitStatus{}, err)
		return
	}
	spec.Stdout = out

	errOut, drainErr, err := outputPipe(id, "stderr", bufWriter{buf: session.buf, stderr: true})
	if err != nil {
		s.endPipes(session, spec.Stdin, models.ExitStatus{}, errors.Join(err, out.Close()))
		return
	}
	spec.Stderr = errOut

	exit, execErr := s.cfg.Provider.Exec(ctx, id, spec)

	// Our copy of each write end keeps its pipe readable, so the output drains only after they go.
	closeErr := errors.Join(out.Close(), errOut.Close())
	drainErrs := errors.Join(<-drainOut, <-drainErr)

	s.endPipes(session, spec.Stdin, exit, errors.Join(execErr, closeErr, drainErrs))
}

// endPipes closes the command's stdin and buffer once, then records how it ended.
func (s *Service) endPipes(session *execSession, stdin *os.File, exit models.ExitStatus, err error) {
	var stdinErr error
	if stdin != nil {
		stdinErr = errors.Join(session.closeStdin(), stdin.Close())
	}

	session.buf.close()
	session.setResult(exit, errors.Join(err, stdinErr))
}

// runTerminal runs a tty command, merging its one stream into the buffer, and records its exit.
func (s *Service) runTerminal(ctx context.Context, id string, session *execSession, spec models.ExecSpec) {
	defer session.cancel()

	pair := session.pair
	drained := make(chan error, 1)
	go func() {
		drained <- copyStream(bufWriter{buf: session.buf}, pair.Master)
	}()

	exit, execErr := s.cfg.Provider.Exec(ctx, id, spec)

	// Closing the replica lets the master read EOF, so the copier ends.
	closeErr := pair.Replica.Close()
	pair.Replica = nil

	// A process the command left behind holds the replica too, and then nothing ever ends the copy.
	var drainErr error
	select {
	case drainErr = <-drained:
	case <-time.After(drainBudget):
	}

	masterErr := pair.Master.Close()

	session.buf.close()
	session.setResult(exit, errors.Join(execErr, closeErr, drainErr, masterErr))
}

// Attach replays the buffer so far to one client, then streams live until the command ends. A client that
// drops returns its own context error and leaves the command running, so a later attach replays it again.
func (s *Service) Attach(ctx context.Context, ref, execID string, streams Streams) (models.ExitStatus, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.ExitStatus{}, err
	}

	session, err := s.execOf(id, execID)
	if err != nil {
		return models.ExitStatus{}, err
	}

	if err := session.tryAttach(); err != nil {
		return models.ExitStatus{}, err
	}
	defer session.endAttach()

	if err := started(streams, execID); err != nil {
		return models.ExitStatus{}, err
	}

	s.pumpStdin(session, streams)

	sink := func(c chunk) error {
		if session.tty || !c.stderr {
			return writeChunk(streams.Stdout, c.data)
		}

		return writeChunk(streams.Stderr, c.data)
	}

	if err := session.buf.stream(ctx, sink); err != nil {
		return models.ExitStatus{}, err
	}

	// The buffer closed, so the command ended; answer the live client with how it did.
	<-session.done

	return session.result()
}

// pumpStdin feeds the client's keyboard to the command while the attach lasts. On a clean end it closes
// the command's stdin, so an explicit end of input is a stream-4; a drop leaves stdin open for a re-attach.
func (s *Service) pumpStdin(session *execSession, streams Streams) {
	if streams.Stdin == nil {
		return
	}

	// A command created without stdin reads nothing, so what a tty-less client types is discarded, not stalled.
	if !session.stdinReq && !session.tty {
		go func() {
			warn(streams.Warn, copyStream(io.Discard, streams.Stdin), "the keyboard of a command without stdin")
		}()

		return
	}

	target := session.stdinTarget()
	go func() {
		err := copyStream(target, streams.Stdin)
		if err == nil {
			warn(streams.Warn, session.closeStdin(), "the command was not told its input had ended")
			return
		}

		warn(streams.Warn, err, "the keyboard stopped reaching the command")
	}()
}

// GetExec answers the exec record now, without waiting for it to end.
func (s *Service) GetExec(_ context.Context, ref, execID string) (models.Exec, error) {
	session, err := s.execFor(ref, execID)
	if err != nil {
		return models.Exec{}, err
	}

	return session.record(), nil
}

// WaitExec blocks until the exec ends, then answers its record, so a client learns the exit without a stream.
func (s *Service) WaitExec(ctx context.Context, ref, execID string) (models.Exec, error) {
	session, err := s.execFor(ref, execID)
	if err != nil {
		return models.Exec{}, err
	}

	select {
	case <-session.done:
	case <-ctx.Done():
		return models.Exec{}, ctx.Err()
	}

	return session.record(), nil
}

// ListExecs answers every exec of one sandbox, sorted by id so a page cursor is a stable position.
func (s *Service) ListExecs(_ context.Context, ref string) ([]models.Exec, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return nil, err
	}

	// The sandbox must exist, so a list of an id nobody holds is a 404, not an empty page.
	if _, err := s.cfg.Repo.Get(id); err != nil {
		return nil, err
	}

	return s.execsOf(id), nil
}

// KillExec sends one signal to a running exec. The default is TERM, and a finished exec is refused.
func (s *Service) KillExec(ctx context.Context, ref, execID, signal string) error {
	sig, err := normalizeSignal(signal)
	if err != nil {
		return err
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	session, err := s.execOf(id, execID)
	if err != nil {
		return err
	}

	select {
	case <-session.done:
		return &ExecExitedError{ID: execID}
	default:
	}

	pid, err := session.waitPID(ctx)
	if err != nil {
		return err
	}

	return s.cfg.Provider.Signal(ctx, id, pid, sig)
}

// DeleteExec forgets an exec that has ended and frees its buffer. An exec still running is refused.
func (s *Service) DeleteExec(_ context.Context, ref, execID string) error {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	session, err := s.execOf(id, execID)
	if err != nil {
		return err
	}

	select {
	case <-session.done:
	default:
		return &ExecRunningError{ID: execID}
	}

	session.cancel()
	s.dropExec(execID)

	return nil
}

// ResizeExec sets the window of one exec that runs on a terminal, which is what a SIGWINCH forwards.
func (s *Service) ResizeExec(_ context.Context, ref, execID string, size TerminalSize) error {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	session, err := s.execOf(id, execID)
	if err != nil {
		return err
	}

	// An exec that ended, or one that runs on pipes, has no terminal to resize.
	if session.pair == nil {
		return fmt.Errorf("exec %s of sandbox %s: %w", execID, id, sandboxstate.ErrNotFound)
	}
	select {
	case <-session.done:
		return fmt.Errorf("exec %s of sandbox %s: %w", execID, id, sandboxstate.ErrNotFound)
	default:
	}

	return session.pair.Resize(pty.Size{Rows: size.Rows, Cols: size.Cols})
}

// dropExecs ends and forgets every exec of one sandbox, because a stop takes its execs with it.
func (s *Service) dropExecs(id string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	for execID, session := range s.execs {
		if session.sandboxID != id {
			continue
		}

		session.cancel()
		delete(s.execs, execID)
	}
}

// execFor resolves a reference and finds one of its execs, the path a get and a wait share.
func (s *Service) execFor(ref, execID string) (*execSession, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return nil, err
	}

	return s.execOf(id, execID)
}

func (s *Service) execOf(id, execID string) (*execSession, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	session := s.execs[execID]
	if session == nil || session.sandboxID != id {
		return nil, fmt.Errorf("exec %s of sandbox %s: %w", execID, id, sandboxstate.ErrNotFound)
	}

	return session, nil
}

func (s *Service) execsOf(id string) []models.Exec {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	var out []models.Exec
	for _, session := range s.execs {
		if session.sandboxID == id {
			out = append(out, session.record())
		}
	}

	slices.SortFunc(out, func(a, b models.Exec) int { return strings.Compare(a.ID, b.ID) })

	return out
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

// exitedExecCap bounds the exited execs one sandbox retains, so a sandbox that runs many commands in a
// loop does not grow without a bound.
const exitedExecCap = 32

// capExitedExecs keeps at most exitedExecCap exited execs for one sandbox and drops the oldest, so an
// evicted exec answers 404 like a deleted one. A running exec never counts and is never evicted.
func (s *Service) capExitedExecs(sandboxID string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	type aged struct {
		session *execSession
		at      time.Time
	}

	var exited []aged
	for _, session := range s.execs {
		if session.sandboxID != sandboxID {
			continue
		}
		if done, at := session.exited(); done {
			exited = append(exited, aged{session: session, at: at})
		}
	}
	if len(exited) <= exitedExecCap {
		return
	}

	slices.SortFunc(exited, func(a, b aged) int { return a.at.Compare(b.at) })
	for _, e := range exited[:len(exited)-exitedExecCap] {
		e.session.cancel()
		delete(s.execs, e.session.id)
	}
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
		return "", &StateError{ID: id, State: sb.State, Fix: "start it again with shard start " + id, Code: models.CodeSandboxNotRunning}
	}

	// The provider holds nothing of a paused sandbox, and gone is the wrong word for one a resume brings back.
	if sb.State == models.StatePaused {
		return "", &StateError{ID: id, State: sb.State, Fix: "resume it with shard resume " + id, Code: models.CodeSandboxNotRunning}
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
		return "", &UnavailableError{ID: id, Why: OOMKilledReason, Fix: fmt.Sprintf("remove it with shard rm %s and create another with a larger --memory", id)}
	}

	if !status.Exists {
		return "", &UnavailableError{ID: id, Why: "is gone from " + s.cfg.Provider.Name(), Fix: fmt.Sprintf("remove it with shard rm %s and create another", id)}
	}

	return "", &StateError{ID: id, State: status.State, Fix: "start it again with shard start " + id, Code: models.CodeSandboxNotRunning}
}

// outputPipe copies one of the guest's streams into the buffer and reports what stopped the copy.
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

func specOf(req ExecRequest) models.ExecSpec {
	return models.ExecSpec{
		Argv:    req.Command,
		Env:     req.Env,
		WorkDir: req.WorkDir,
		User:    req.User,
		TTY:     req.TTY,
	}
}

// signalOf is the two signals a kill accepts, and the empty string that means the default.
var signalOf = map[string]string{"": "TERM", "TERM": "TERM", "KILL": "KILL"}

// normalizeSignal maps the request's signal to the name the provider signals by, or refuses an unknown one.
func normalizeSignal(signal string) (string, error) {
	sig, ok := signalOf[signal]
	if !ok {
		return "", &RequestError{Err: fmt.Errorf("the signal %q is not one of TERM or KILL", signal)}
	}

	return sig, nil
}

// ValidExecID says whether s could be an exec id, so a list cursor that could name no exec is refused.
func ValidExecID(s string) error {
	if len(s) != execIDLen {
		return fmt.Errorf("an exec id is %d hex characters, got %q", execIDLen, s)
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("an exec id is hex, got %q", s)
		}
	}

	return nil
}

// newExecID names one exec for as long as the daemon holds it, which is what every exec route reaches it by.
func newExecID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes for an exec id: %w", err)
	}

	return fmt.Sprintf("%x", b), nil
}

// writeChunk sends one buffered write to the client, skipping a stream the attach did not ask for.
func writeChunk(w io.Writer, data []byte) error {
	if w == nil {
		return nil
	}

	if _, err := w.Write(data); err != nil {
		return err
	}

	return nil
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

// started tells the caller the exec exists, so it can name it to the client before the replay begins.
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
