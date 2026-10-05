package sandbox

import (
	"cmp"
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

// ExecStallBound is how long a write waits on an attached client that takes no output before the daemon detaches it.
const ExecStallBound = 30 * time.Second

// DefaultExecStartBudget bounds the wait for a command's launch, well under the client's 30 s call timeout.
const DefaultExecStartBudget = 20 * time.Second

// DefaultExecCleanupGrace is how long a launch past its budget gets to end once cancelled, so the 504 lands by 22 s.
const DefaultExecCleanupGrace = 2 * time.Second

// execIDLen is how many hex characters name an exec, so a list cursor that is not one is refused.
const execIDLen = 16

// The execs the daemon runs at once, per sandbox and in all: each holds up to execBufferCap of output in daemon memory, outside every sandbox's bound (SHARD-550).
const (
	maxRunningExecsPerSandbox = 32
	maxRunningExecs           = 256
)

// ExecRequest is one command to run in a sandbox that already runs. It is the body of POST /v0/sandboxes/{id}/exec.
type ExecRequest struct {
	Command []string `json:"command" minItems:"1"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	// Stdin says the client will type at the command; without it the command reads nothing at all.
	Stdin bool `json:"stdin,omitempty"`
	// TTY gives the command a terminal in the guest, which the daemon holds the other end of.
	TTY bool `json:"tty,omitempty"`
	// Size is the terminal the command starts on, and a resize replaces it.
	Size TerminalSize `json:"size,omitzero"`
	// Attach says a client attaches right after the create, so the output waits for it rather than being evicted.
	Attach bool `json:"attach,omitempty"`
}

// TerminalSize is a terminal window in character cells. It is the body of the resize route too.
type TerminalSize struct {
	Rows uint16 `json:"rows" required:"false" maximum:"65535"`
	Cols uint16 `json:"cols" required:"false" maximum:"65535"`
}

// Streams is where one attach's stdio goes. The caller owns them: a nil Stdin is a client that types nothing.
type Streams struct {
	Stdin io.Reader
	// StopStdin must unblock a caller-owned Stdin read when the attach ends.
	StopStdin func() error
	Stdout    io.Writer
	Stderr    io.Writer
	// Started is called with the exec id before the replay begins, and its error ends the attach.
	Started func(execID string) error
	// Detach ends the attach from the daemon's side, so a write blocked on a client that stopped reading returns.
	Detach func()
}

// Attached is how one attach ended: the command's exit, and the output the buffer evicted before any client took it.
type Attached struct {
	Exit      models.ExitStatus
	LostBytes int64
}

// UnavailableError is a sandbox no command can run in, because the substrate no longer holds it.
type UnavailableError struct {
	ID string
	// Why is what became of the sandbox, and Fix what the operator does about it.
	Why string
	Fix string
	// Detail is host context, such as the pid that missed its probe, which only the local text carries.
	Detail string
}

func (e *UnavailableError) Error() string {
	if e.Detail == "" {
		return e.Public()
	}

	return fmt.Sprintf("sandbox %s %s: %s: %s", e.ID, e.Why, e.Detail, e.Fix)
}

func (e *UnavailableError) Public() string {
	return fmt.Sprintf("sandbox %s %s: %s", e.ID, e.Why, e.Fix)
}

// AttachedError is an exec a client already watches: one attach at a time replays and streams its output.
type AttachedError struct {
	ID string
}

func (e *AttachedError) Error() string {
	return fmt.Sprintf("exec %s is already attached: wait for that client to leave", e.ID)
}

func (e *AttachedError) Public() string { return e.Error() }

// StalledError is an attach the daemon detached because its client took no output for ExecStallBound.
type StalledError struct {
	ID string
}

func (e *StalledError) Error() string {
	return fmt.Sprintf("exec %s: the client took no output for %s, so the daemon detached it and the command runs on", e.ID, ExecStallBound)
}

func (e *StalledError) Public() string { return e.Error() }

// errStalled is the stream's word for a follower the buffer detached, which Attach names with the exec id.
var errStalled = errors.New("the client stalled")

// ExecExitedError is a kill of an exec that already ended, which has no process left to signal.
type ExecExitedError struct {
	ID string
}

func (e *ExecExitedError) Error() string {
	return fmt.Sprintf("exec %s has exited: nothing left to signal", e.ID)
}

func (e *ExecExitedError) Public() string { return e.Error() }

// ExecRunningError is a delete of an exec still under way, whose buffer the daemon must keep.
type ExecRunningError struct {
	ID string
}

func (e *ExecRunningError) Error() string {
	return fmt.Sprintf("exec %s is still running: kill it or wait for it before you delete it", e.ID)
}

func (e *ExecRunningError) Public() string { return e.Error() }

// ExecLimitError is a create past the execs one sandbox, or the whole daemon, runs at once. No running exec is evicted to make room.
type ExecLimitError struct {
	ID    string
	Limit int
	// Daemon is a refusal by the bound across every sandbox, not by the sandbox's own.
	Daemon bool
}

func (e *ExecLimitError) Error() string {
	if e.Daemon {
		return fmt.Sprintf("the daemon runs %d execs, the most it runs at once across all sandboxes: wait for one to exit, or kill one", e.Limit)
	}

	return fmt.Sprintf("sandbox %s runs %d execs, the most one sandbox runs at once: wait for one to exit, or kill one", e.ID, e.Limit)
}

func (e *ExecLimitError) Public() string { return e.Error() }

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
	// released opens the buffer to a stream; discarded drops a not-started command's output before then.
	released  bool
	discarded bool

	// follower is the client a write waits for rather than evict what it has not taken.
	follower *follower
	stall    time.Duration
	// evicted is the offset of the oldest byte held, taken the furthest any client read to, lost what none read.
	evicted int64
	taken   int64
	lost    int64
	// waits counts the writes that had to wait for the follower, so a drain tells a slow client from a dead copier.
	waits   uint64
	waiting int
}

// follower is one client a write waits for: an attach, or the hold that keeps the output for the first one.
type follower struct {
	// detach ends the attach from the daemon's side; the hold has no attach to end.
	detach  func()
	stalled bool
	// at is the offset the client accepted the output up to, and progress when it last accepted a chunk.
	at       int64
	progress time.Time
}

// newExecBuffer answers an empty buffer; hold keeps the output for a client that attaches right after the create.
func newExecBuffer(hold bool) *execBuffer {
	b := &execBuffer{changed: make(chan struct{}), stall: ExecStallBound}
	if hold {
		b.follower = &follower{}
	}

	return b
}

// append copies one write in and evicts the oldest chunks over the cap, once a follower with no room took what it owes.
func (b *execBuffer) append(data []byte, stderr bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.await(len(data))

	kept := make([]byte, len(data))
	copy(kept, data)
	b.chunks = append(b.chunks, chunk{data: kept, stderr: stderr})
	b.size += len(kept)

	for b.size > execBufferCap && len(b.chunks) > 1 {
		n := len(b.chunks[0].data)
		if b.evicted >= b.taken {
			b.lost += int64(n)
		}
		b.evicted += int64(n)
		b.size -= n
		b.chunks[0] = chunk{}
		b.chunks = b.chunks[1:]
		b.dropped++
		b.truncated = true
	}

	b.wake()
}

// full says a write of n bytes would evict what the follower has not taken; before the release no stream can take any.
func (b *execBuffer) full(n int) bool {
	if !b.released || b.follower == nil {
		return false
	}
	owed := b.owed()

	return owed > 0 && owed+int64(n) > execBufferCap
}

// owed is what the buffer holds past the follower's offset; what it evicted before the follower took it is lost, not owed.
func (b *execBuffer) owed() int64 {
	return b.evicted + int64(b.size) - max(b.follower.at, b.evicted)
}

// await holds a write until the follower takes what it owes, and detaches one that accepted nothing for the stall bound.
func (b *execBuffer) await(n int) {
	if !b.full(n) {
		return
	}

	b.waits++
	b.waiting++
	defer func() { b.waiting-- }()

	start := time.Now()
	timer := time.NewTimer(b.stall)
	defer timer.Stop()

	for b.full(n) {
		// The bound runs from the wait or the follower's last accepted chunk, whichever is later.
		since := start
		if b.follower.progress.After(since) {
			since = b.follower.progress
		}
		left := b.stall - time.Since(since)
		if left <= 0 {
			b.detach()

			continue
		}
		timer.Reset(left)

		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
		}
		b.mu.Lock()
	}
}

// detach stops waiting for a follower that accepted nothing for the stall bound, and ends its attach.
func (b *execBuffer) detach() {
	f := b.follower
	f.stalled = true
	b.follower = nil
	if f.detach != nil {
		f.detach()
	}

	b.wake()
}

// follow makes a stream's client the one writes wait for, in place of the hold, owing all the buffer holds.
func (b *execBuffer) follow(detach func()) *follower {
	b.mu.Lock()
	defer b.mu.Unlock()

	f := &follower{detach: detach, progress: time.Now()}
	b.follower = f

	return f
}

// unfollow lets the writes run free again once the client is gone, evicting what nobody takes.
func (b *execBuffer) unfollow(f *follower) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.follower != f {
		return
	}
	b.follower = nil
	b.wake()
}

// take records that f's client accepted the output up to at, so a write waiting on it has room again and a fresh bound.
func (b *execBuffer) take(f *follower, at int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.taken = max(b.taken, at)
	f.at, f.progress = at, time.Now()
	if b.follower == f && b.waiting > 0 {
		b.wake()
	}
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

func (b *execBuffer) lostBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.lost
}

// waited answers how many writes have waited for a client and whether one waits now.
func (b *execBuffer) waited() (uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.waits, b.waiting > 0
}

// release opens the buffer to a stream, so the output a running command made becomes visible.
func (b *execBuffer) release() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.discarded {
		return
	}
	b.released = true
	b.wake()
}

// discard drops the output of a command that never started, so its raw substrate error never streams.
func (b *execBuffer) discard() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.released {
		return
	}
	b.discarded = true
	b.chunks = nil
	b.size = 0
	b.wake()
}

// wake replaces the change channel so every waiter unblocks. The caller holds the lock.
func (b *execBuffer) wake() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// stream replays what the buffer holds from the oldest byte, then follows live until the command ends
// or the context is cancelled. A cancelled context is a client that left, not the command ending.
func (b *execBuffer) stream(ctx context.Context, detach func(), emit func(chunk) error) error {
	f := b.follow(detach)
	defer b.unfollow(f)

	next := 0
	var at int64
	for {
		b.mu.Lock()
		if f.stalled {
			b.mu.Unlock()
			return errStalled
		}

		var batch []chunk
		if b.released {
			if next < b.dropped {
				next, at = b.dropped, b.evicted
			}
			pending := b.chunks[next-b.dropped:]
			batch = make([]chunk, len(pending))
			copy(batch, pending)
			next += len(batch)
		}
		closed := b.closed
		changed := b.changed
		b.mu.Unlock()

		for _, c := range batch {
			if err := emit(c); err != nil {
				return b.cause(f, err)
			}
			// Only a chunk the client accepted is taken, so one cut midway leaves the rest owed and counted lost.
			at += int64(len(c.data))
			b.take(f, at)
		}

		if closed {
			return nil
		}

		select {
		case <-ctx.Done():
			return b.cause(f, ctx.Err())
		case <-changed:
		}
	}
}

// cause names a follower the buffer detached for its stall, rather than the write or context that broke with it.
func (b *execBuffer) cause(f *follower, err error) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if f.stalled {
		return errStalled
	}

	return err
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

// execSession owns the command context so a client disconnect cannot end the command.
type execSession struct {
	id        string
	sandboxID string
	command   []string
	tty       bool
	stdinReq  bool
	startedAt time.Time

	cancel context.CancelFunc
	done   chan struct{}
	// shown says the create answered this exec, so get, list and the cap see it; guarded by the service's execMu.
	shown bool

	buf *execBuffer

	// pid is the provider's process handle; pidSet closes once the provider reports it.
	pidMu   sync.Mutex
	pid     int
	pidOnce sync.Once
	pidSet  chan struct{}

	// stdinW is the write end of a non-tty command's stdin; pair is the terminal of a tty command.
	stdinW      *os.File
	stdinOnce   sync.Once
	pair        *pty.Pty
	inputMu     sync.Mutex
	inputClosed bool
	// resizes holds the latest window for a provider whose guest owns the pty; one slot, since only the last size matters.
	resizes chan models.TerminalSize

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
		LostBytes: e.buf.lostBytes(),
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

	// A reported pid means the command runs, so its output may stream.
	e.buf.release()
	e.pidOnce.Do(func() { close(e.pidSet) })
}

// reported says the provider gave the command's pid, so the command started.
func (e *execSession) reported() bool {
	select {
	case <-e.pidSet:
		return true
	default:
		return false
	}
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
		// The provider knows the reason, and the session the program the caller named.
		named := *notStarted
		named.Command = e.command[0]
		e.startErr = &named
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

// settleBuffer opens the buffer on a command that ran, or discards a start-failure's or a pause refusal's held output.
func (e *execSession) settleBuffer(err error) {
	_, notStarted := errors.AsType[*models.CommandNotStartedError](err)
	refused, ok := errors.AsType[*StateError](err)
	if notStarted || ok && refused.State == models.StatePaused {
		e.buf.discard()
		e.buf.close()
		return
	}

	e.buf.release()
	e.buf.close()
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
func (e *execSession) stdinTarget() *os.File {
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
	e.stdinOnce.Do(func() {
		e.inputMu.Lock()
		defer e.inputMu.Unlock()
		e.inputClosed = true
		err = e.stdinW.Close()
	})

	return err
}

// CreateExec waits for launch proof so a refused command does not receive a public handle.
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

	if err := s.admitExec(id); err != nil {
		return models.Exec{}, err
	}
	session, err := s.startExec(id, execID, req)
	if err != nil {
		s.releaseExec(id)
		return models.Exec{}, err
	}
	// The slot frees when the command ends, whether it ran, never started, or a stop or a remove ended it.
	go func() {
		<-session.done
		s.releaseExec(id)
	}()

	// Held before the wait so a stop cancels it, and shown only once it launched, so a refused command never lists.
	s.holdExec(execID, session)
	if err := s.awaitStart(ctx, session); err != nil {
		return models.Exec{}, err
	}
	s.showExec(session)
	s.capExitedExecs(id)

	return session.record(), nil
}

// awaitStart answers once the launch is decided, which runs apart from ctx so a client that goes cannot lift the budget.
func (s *Service) awaitStart(ctx context.Context, session *execSession) error {
	decided := make(chan error, 1)
	go func() { decided <- s.superviseStart(session) }()

	select {
	case err := <-decided:
		return err
	case <-ctx.Done():
		// A client that goes never ends the remote command, so it runs on as a listed exec.
		s.showExec(session)
		return ctx.Err()
	}
}

// superviseStart returns once the provider reports the pid, which every provider does only after the command's execve took.
func (s *Service) superviseStart(session *execSession) error {
	budget := s.execStartBudget()
	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case <-session.pidSet:
		return nil
	case <-session.done:
		return s.startOutcome(session)
	case <-timer.C:
	}
	if session.reported() {
		return nil
	}

	session.cancel()
	grace := time.NewTimer(s.execCleanupGrace())
	defer grace.Stop()

	select {
	case <-session.done:
		s.dropHidden(session)
	case <-grace.C:
		// A provider that ignores the cancel keeps the hidden session until it ends, so a stop still finds it.
		go func() {
			<-session.done
			s.dropHidden(session)
		}()
	}

	return &SubstrateTimeoutError{ID: session.sandboxID, Op: "exec", Budget: budget}
}

// startOutcome answers an exec that ended before its create did: a fast command that launched, or why none did.
func (s *Service) startOutcome(session *execSession) error {
	if session.reported() {
		return nil
	}

	s.dropHidden(session)
	if _, err := session.result(); err != nil {
		return userRefused(err)
	}

	return fmt.Errorf("the exec in sandbox %s ended with no report that its command launched", session.sandboxID)
}

func (s *Service) execStartBudget() time.Duration {
	if s.cfg.ExecStartBudget != 0 {
		return s.cfg.ExecStartBudget
	}

	return DefaultExecStartBudget
}

func (s *Service) execCleanupGrace() time.Duration {
	if s.cfg.ExecCleanupGrace != 0 {
		return s.cfg.ExecCleanupGrace
	}

	return DefaultExecCleanupGrace
}

// startExec opens the command's stdio and runs it in the background, so the create waits on its launch with a bound.
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
		buf:       newExecBuffer(req.Attach),
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
		session.resizes = make(chan models.TerminalSize, 1)
		spec.Resizes = session.resizes
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
	execErr = s.refusedByPause(id, session, execErr)
	execErr = s.endedUnderExec(id, session, execErr)

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

	combined := errors.Join(err, stdinErr)
	session.settleBuffer(combined)
	session.setResult(exit, combined)
	// Cap on exit too, so execs that exit with no following create still settle at the retained cap.
	s.capExitedExecs(session.sandboxID)
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
	execErr = s.refusedByPause(id, session, execErr)
	execErr = s.endedUnderExec(id, session, execErr)

	// Closing the replica lets the master read EOF, so the copier ends.
	closeErr := pair.Replica.Close()
	pair.Replica = nil

	// A process the command left behind holds the replica too, and then nothing ever ends the copy.
	drainErr := drain(drained, session.buf)

	masterErr := session.closeTerminalMaster()

	combined := errors.Join(execErr, closeErr, drainErr, masterErr)
	session.settleBuffer(combined)
	session.setResult(exit, combined)
	s.capExitedExecs(id)
}

// drain waits for the terminal's copier, longer only while a write waits on a client that still takes output.
func drain(drained <-chan error, buf *execBuffer) error {
	last, _ := buf.waited()
	for {
		select {
		case err := <-drained:
			return err
		case <-time.After(drainBudget):
		}

		waits, waiting := buf.waited()
		if !waiting && waits == last {
			return nil
		}
		last = waits
	}
}

// Attach replays the buffer so far to one client, then streams live until the command ends. A client that drops
// returns its context error, and one that takes no output for ExecStallBound a StalledError; the command runs on.
func (s *Service) Attach(ctx context.Context, ref, execID string, streams Streams) (attached Attached, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id, _, err := s.resolveForExec(ref)
	if err != nil {
		return Attached{}, err
	}

	session, err := s.execOf(id, execID)
	if err != nil {
		return Attached{}, err
	}

	if err := session.tryAttach(); err != nil {
		return Attached{}, err
	}
	defer session.endAttach()

	if err := started(streams, execID); err != nil {
		return Attached{}, err
	}

	stopInput := startStdin(ctx, session, streams, cancel)
	defer func() { err = errors.Join(err, stopInput()) }()

	sink := func(c chunk) error {
		if session.tty || !c.stderr {
			return writeChunk(streams.Stdout, c.data)
		}

		return writeChunk(streams.Stderr, c.data)
	}

	err = session.buf.stream(ctx, streams.Detach, sink)
	if errors.Is(err, errStalled) {
		return Attached{}, &StalledError{ID: execID}
	}
	if err != nil {
		return Attached{}, err
	}

	// The buffer closed, so the command ended; answer the live client with how it did.
	<-session.done

	exit, err := session.result()
	if err != nil {
		return Attached{}, err
	}

	return Attached{Exit: exit, LostBytes: session.buf.lostBytes()}, nil
}

func startStdin(ctx context.Context, session *execSession, streams Streams, detach context.CancelFunc) func() error {
	if streams.Stdin == nil {
		return func() error { return nil }
	}
	ctx, cancel := context.WithCancel(ctx)
	copied := make(chan error, 1)
	go func() {
		err := pumpStdin(ctx, session, streams)
		if err != nil {
			detach()
		}
		copied <- err
	}()
	return func() error {
		cancel()
		return <-copied
	}
}

// pumpStdin joins its interrupt before a later attach can write to the same command.
func pumpStdin(ctx context.Context, session *execSession, streams Streams) (err error) {
	target := session.stdinTarget()
	if !session.stdinReq && !session.tty {
		target = nil
	}

	interrupted := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		var deadlineErr, sourceErr error
		if target != nil {
			deadlineErr = session.inputDeadline(time.Now())
		}
		if streams.StopStdin != nil {
			sourceErr = streams.StopStdin()
		}
		interrupted <- errors.Join(deadlineErr, sourceErr)
	})
	defer func() {
		if !stop() {
			err = errors.Join(err, <-interrupted)
		}
		if target != nil {
			err = errors.Join(err, session.inputDeadline(time.Time{}))
		}
	}()

	dst := io.Discard
	if target != nil {
		dst = target
	}
	err = copyStream(dst, streams.Stdin)
	if ctx.Err() != nil && (errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("copy the exec input: %w", err)
	}
	if ctx.Err() != nil || target == nil {
		return nil
	}

	return session.closeStdin()
}

func (e *execSession) inputDeadline(deadline time.Time) error {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.inputClosed {
		return nil
	}
	if err := e.stdinTarget().SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set the exec input deadline: %w", err)
	}
	return nil
}

func (e *execSession) closeTerminalMaster() error {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	e.inputClosed = true
	return e.pair.Master.Close()
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
	id, _, err := s.resolveForExec(ref)
	if err != nil {
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

	id, _, err := s.resolveForExec(ref)
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

	err = s.cfg.Provider.Signal(ctx, id, pid, sig)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return &ExecExitedError{ID: execID}
	}

	return err
}

// DeleteExec forgets an exec that has ended and frees its buffer. An exec still running is refused.
func (s *Service) DeleteExec(_ context.Context, ref, execID string) error {
	id, _, err := s.resolveForExec(ref)
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
	id, _, err := s.resolveForExec(ref)
	if err != nil {
		return err
	}

	session, err := s.execOf(id, execID)
	if err != nil {
		return err
	}

	// An exec that ended, or one that runs on pipes, has no terminal to resize.
	if session.pair == nil {
		return models.NotFound(sandboxstate.ErrNotFound, fmt.Sprintf("exec %s of sandbox %s has no terminal to resize; only an exec created with tty has one", execID, id))
	}
	select {
	case <-session.done:
		return models.NotFound(sandboxstate.ErrNotFound, fmt.Sprintf("exec %s of sandbox %s has ended; it has no terminal to resize", execID, id))
	default:
	}

	if err := session.pair.Resize(pty.Size{Rows: size.Rows, Cols: size.Cols}); err != nil {
		return err
	}
	offer(session.resizes, models.TerminalSize{Rows: size.Rows, Cols: size.Cols})

	return nil
}

// offer replaces whatever size waits in the slot, so a provider that reads late gets the current window and never blocks a resize.
func offer(slot chan models.TerminalSize, size models.TerminalSize) {
	for {
		select {
		case slot <- size:
			return
		default:
		}
		select {
		case <-slot:
		default:
		}
	}
}

// dropExecs ends and forgets every exec of one sandbox, because a sandbox that ends takes its execs with it.
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
	id, _, err := s.resolveForExec(ref)
	if err != nil {
		return nil, err
	}

	return s.execOf(id, execID)
}

func (s *Service) execOf(id, execID string) (*execSession, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	session := s.execs[execID]
	if session == nil || session.sandboxID != id || !session.shown {
		return nil, models.NotFound(sandboxstate.ErrNotFound, fmt.Sprintf("exec %s not found in sandbox %s", execID, id))
	}

	return session, nil
}

func (s *Service) execsOf(id string) []models.Exec {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	var out []models.Exec
	for _, session := range s.execs {
		if session.sandboxID == id && session.shown {
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

func (s *Service) showExec(session *execSession) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	session.shown = true
}

func (s *Service) dropExec(execID string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	delete(s.execs, execID)
}

// dropHidden forgets an exec whose create refused it, and keeps one a client left before the answer, as a listed record.
func (s *Service) dropHidden(session *execSession) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	if !session.shown {
		delete(s.execs, session.id)
	}
}

// admitExec takes one running slot of sandbox id, or refuses when the sandbox or the daemon has none left.
func (s *Service) admitExec(id string) error {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	if s.running[id] >= maxRunningExecsPerSandbox {
		return &ExecLimitError{ID: id, Limit: maxRunningExecsPerSandbox}
	}
	if s.runningAll >= maxRunningExecs {
		return &ExecLimitError{ID: id, Limit: maxRunningExecs, Daemon: true}
	}
	s.running[id]++
	s.runningAll++

	return nil
}

// releaseExec frees the slot admitExec took for sandbox id.
func (s *Service) releaseExec(id string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	s.runningAll--
	s.running[id]--
	if s.running[id] == 0 {
		delete(s.running, id)
	}
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
		if session.sandboxID != sandboxID || !session.shown {
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

// resolveForExec resolves the reference and refuses a failed sandbox, so every exec verb answers 409
// sandbox_failed on one, the same as the contract gives every verb but a delete of the sandbox itself.
func (s *Service) resolveForExec(ref string) (string, models.Sandbox, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", models.Sandbox{}, err
	}

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return "", models.Sandbox{}, err
	}

	if err := FailedGuard(id, sb); err != nil {
		return "", models.Sandbox{}, err
	}

	return id, sb, nil
}

// readyForExec resolves the reference and refuses a sandbox no command can run in. The record
// answers for an id nobody created, and the substrate for the state, because a record saying
// running outlives a host restart.
func (s *Service) readyForExec(ctx context.Context, ref string) (string, error) {
	id, sb, err := s.resolveForExec(ref)
	if err != nil {
		return "", err
	}

	// A record that says stopped outranks the oom count the cgroup kept, and a paused one never has a cgroup.
	if sb.State == models.StateStopped {
		return "", &StateError{ID: id, State: sb.State, Fix: "start it again with shard start " + nameOf(id, sb), Code: models.CodeSandboxNotRunning}
	}

	// The provider holds nothing of a paused sandbox, and gone is the wrong word for one a resume brings back.
	if sb.State == models.StatePaused {
		return "", pausedRefusal(id, sb)
	}

	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		// A pause removes the substrate's state before its record says paused, so its question fails mid-pause.
		return "", s.pauseOutranks(id, err)
	}
	// The substrate is asked even for an unresponsive record, so an exec works as soon as the process answers again.
	if status.State == models.StateUnresponsive {
		if err := s.noteUnresponsive(id, sb, status.Reason); err != nil {
			return "", err
		}

		return "", &UnavailableError{ID: id, Why: "is unresponsive", Detail: status.Reason, Fix: "wait for it to answer, or end it with shard stop " + nameOf(id, sb)}
	}
	if status.Alive() {
		return id, nil
	}

	// A pause ends the substrate's run before its record says paused, so the record read again names that pause and no stop (SHARD-478).
	paused, err := s.pausedMeanwhile(id)
	if err != nil {
		return "", err
	}
	if paused {
		return "", pausedRefusal(id, sb)
	}

	// The exit file records a 137 for this, which is what a plain kill -9 records too, so the reason
	// is named here or an operator never learns it.
	if status.OOMKilled {
		return "", &UnavailableError{ID: id, Why: OOMKilledReason, Fix: fmt.Sprintf("start it again with shard start %s, over the files it kept; more memory needs a new sandbox with a larger resources.memory_mib", nameOf(id, sb))}
	}

	if !status.Exists {
		return "", &UnavailableError{ID: id, Why: "is gone from " + s.cfg.Provider.Name(), Fix: fmt.Sprintf("remove it with shard remove %s and create another sandbox", nameOf(id, sb))}
	}

	return "", &StateError{ID: id, State: status.State, Fix: "start it again with shard start " + nameOf(id, sb), Code: models.CodeSandboxNotRunning}
}

// pausedMeanwhile says a pause that holds no lock against an exec completed since the record was read, recorded or not yet.
func (s *Service) pausedMeanwhile(id string) (bool, error) {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return false, err
	}
	if sb.State == models.StatePaused {
		return true, nil
	}
	dir, err := s.markedCheckpoint(sb)
	if err != nil {
		return false, err
	}

	return dir != "", nil
}

// pausedRefusal is the one text of every exec a pause refuses, whichever layer met the pause first (SHARD-482).
func pausedRefusal(id string, sb models.Sandbox) *StateError {
	return &StateError{ID: id, State: models.StatePaused, Fix: "resume it with shard resume " + nameOf(id, sb), Code: models.CodeSandboxNotRunning}
}

// nameOf is the sandbox as its user knows it: the name they gave it, or its id when it has none.
func nameOf(id string, sb models.Sandbox) string { return cmp.Or(sb.Name, id) }

// refusedByPause names a command that never started inside a pause by that pause, whatever the substrate or the guest said.
func (s *Service) refusedByPause(id string, session *execSession, err error) error {
	// A command that ran keeps its own words, unless the substrate lost its wait, which a pause's teardown causes.
	if err == nil || session.reported() && !errors.Is(err, models.ErrExecLost) {
		return err
	}

	return s.pauseOutranks(id, err)
}

// pauseOutranks swaps err for the one pause text while a pause holds the sandbox.
func (s *Service) pauseOutranks(id string, err error) error {
	sb, getErr := s.cfg.Repo.Get(id)
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	if sb.State != models.StatePaused && !sb.Pausing {
		return err
	}

	return pausedRefusal(id, sb)
}

// endedUnderExec swaps a launch error for not_found or sandbox_not_running when a concurrent stop or remove tore the runtime down under the exec, so a racing rm answers a code, never a 500 (SHARD-563).
func (s *Service) endedUnderExec(id string, session *execSession, err error) error {
	var state *StateError
	var notFound *models.NotFoundError
	if err == nil || session.reported() || errors.As(err, &state) || errors.As(err, &notFound) {
		return err
	}

	// Stop and remove hold the per-sandbox lock until the record is settled, so the read past it is deterministic, never the teardown's own half-written state.
	budget := s.execStartBudget()
	lockCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	unlock, lockErr := s.lock(lockCtx, id)
	// A teardown that holds the lock past the budget is the timeout itself, so name it 504, never a raw 500.
	if lockErr != nil {
		return &SubstrateTimeoutError{ID: id, Op: "exec", Budget: budget}
	}
	defer unlock()

	sb, getErr := s.cfg.Repo.Get(id)
	if errors.Is(getErr, sandboxstate.ErrNotFound) {
		return models.NotFound(sandboxstate.ErrNotFound, fmt.Sprintf("sandbox %s not found", id))
	}
	if getErr != nil {
		return errors.Join(err, getErr)
	}
	if sb.State != models.StateRunning {
		return &StateError{ID: id, State: sb.State, Fix: "start it again with shard start " + nameOf(id, sb), Code: models.CodeSandboxNotRunning}
	}

	return err
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
